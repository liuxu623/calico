// Copyright (c) 2016-2022 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdv3

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/watchersyncer"
)

// Watch entries in the datastore matching the resources specified by the ListInterface.
func (c *etcdV3Client) Watch(cxt context.Context, l model.ListInterface, options api.WatchOptions) (api.WatchInterface, error) {
	var rev int64
	if len(options.Revision) != 0 {
		var err error
		rev, err = strconv.ParseInt(options.Revision, 10, 64)
		if err != nil {
			return nil, err
		}
	}

	wc := &watcher{
		BaseWatcher: watchersyncer.NewBaseWatcher(cxt, log.WithField("list", l)),
		client:      c,
		list:        l,
		initialRev:  rev,
	}
	go wc.watchLoop()
	return wc, nil
}

// ListAndWatch performs list and watch operations for etcd, handling reconnection, resync, and error recovery.
func (c *etcdV3Client) ListAndWatch(ctx context.Context, l model.ListInterface) (<-chan api.WatchEvent, error) {
	log.Debugf("Performing 'ListAndWatch' for %+v", l)
	wc := &watcher{
		BaseWatcher: watchersyncer.NewBaseWatcher(ctx, log.WithField("list", l)),
		client:      c,
		list:        l,
	}
	go wc.runListAndWatch()

	return wc.ResultChan(), nil
}

// watcher implements watch.Interface.
type watcher struct {
	*watchersyncer.BaseWatcher
	client     *etcdV3Client
	initialRev int64
	list       model.ListInterface
	terminated uint32
	watch      api.WatchInterface
}

// PerformList executes the list operation for etcd backend.
func (wc *watcher) PerformList(ctx context.Context, revision string) (*model.KVPairList, error) {
	result, err := wc.client.List(ctx, wc.list, revision)
	if err != nil {
		wc.Logger.WithError(err).Info("Failed to perform list of current data during resync")
		wc.ResyncBlockedUntil = time.Now().Add(watchersyncer.ListRetryInterval)
	}
	return result, err
}

// StartWatch initiates the watch operation for etcd backend.
func (wc *watcher) StartWatch() bool {
	return wc.StartWatchImpl(wc, wc.list)
}

// GetWatch returns the current watch interface.
func (wc *watcher) GetWatch() api.WatchInterface {
	return wc.watch
}

// SetWatch sets the watch interface.
func (wc *watcher) SetWatch(w api.WatchInterface) {
	wc.watch = w
}

// CreateWatch creates a watch from the specified revision with backend-specific options.
func (wc *watcher) CreateWatch(ctx context.Context, list model.ListInterface, revision string) (api.WatchInterface, error) {
	return wc.client.Watch(ctx, list, api.WatchOptions{
		Revision: revision,
	})
}

// HandleWatchError processes watch errors for etcd backend.
// It tracks consecutive errors at the current revision and triggers a full resync
// if the error count exceeds the maximum threshold, which helps recover from
// persistent issues that prevent watch progress.
func (wc *watcher) HandleWatchError(err error) {
	wc.ErrorCountAtCurrentRev++
	if wc.ErrorCountAtCurrentRev >= watchersyncer.MaxErrorsPerRevision {
		wc.Logger.Warn("Watch repeatedly failed without making progress, triggering full resync")
		wc.ResetRevisionForFullResync()
	}
	wc.Logger.Info("Watch of resource finished. Attempting to restart it...")
}

// Stop stops the watcher and releases associated resources.
// This calls through to the context cancel function.
func (wc *watcher) Stop() {
	wc.Cancel()
}

// ResultChan returns a channel used to receive WatchEvents.
func (wc *watcher) ResultChan() <-chan api.WatchEvent {
	return wc.BaseWatcher.ResultChan
}

// HasTerminated returns true when the watcher has completed termination processing.
func (wc *watcher) HasTerminated() bool {
	return atomic.LoadUint32(&wc.terminated) != 0
}

// watchLoop starts a watch on the required path prefix and sends a stream of
// event updates for internal processing.
func (wc *watcher) watchLoop() {
	// When this loop exits, make sure we terminate the watcher resources.
	defer wc.terminateWatcher()

	// If we are not watching a specific resource then this is a prefix watch.
	key, opts := calculateListKeyAndOptions(wc.Logger, wc.list)

	wc.Logger.Debug("Starting watcher.watchLoop")
	if wc.initialRev == 0 {
		// No initial revision supplied, so perform a list of current configuration
		// which will also get the current revision we will start our watch from.
		var kvps *model.KVPairList
		var err error
		if kvps, err = wc.listCurrent(); err != nil {
			wc.Logger.Errorf("failed to list current with latest state: %v", err)
			// Error considered as terminating error, hence terminate watcher.
			wc.sendError(err)
			return
		}

		// If we're handling profiles, filter out the default-allow profile.
		if len(kvps.KVPairs) > 0 && (key == profilesKey || key == defaultAllowProfileKey) {
			wc.removeDefaultAllowProfile(kvps)
		}

		// We are sending an initial sync of entries to the watcher to provide current
		// state.  To the perspective of the watcher, these are added entries, so set the
		// event type to WatchAdded.
		wc.Logger.WithField("NumEntries", len(kvps.KVPairs)).Debug("Sending create events for each existing entry")
		wc.sendAddedEvents(kvps)
	}

	opts = append(opts, clientv3.WithRev(wc.initialRev+1), clientv3.WithPrevKV())
	wc.Logger.WithFields(log.Fields{
		"etcdv3-etcdKey": key,
		"rev":            wc.initialRev,
	}).Debug("Starting etcdv3 watch")
	wch := wc.client.etcdClient.Watch(wc.Ctx, key, opts...)
	for wres := range wch {
		if wres.Err() != nil {
			// A watch channel error is a terminating event, so exit the loop.
			err := wres.Err()
			wc.Logger.WithError(err).Warning("Watch channel error")
			wc.sendError(err)
			return
		}
		for _, e := range wres.Events {
			// Convert the etcdv3 event to the equivalent Watcher event.  An error
			// parsing the event is returned as an error, but don't exit the watcher as
			// restarting the watcher is unlikely to fix the conversion error.
			if ae, err := convertWatchEvent(e, wc.list); ae != nil {
				wc.sendEvent(ae)
			} else if err != nil {
				wc.sendError(err)
			}
		}
	}

	// If we exit the loop, it means the watcher has closed for some reason.
	wc.Logger.Warn("etcdv3 watch channel closed")
}

// listCurrent retrieves the existing entries.
func (wc *watcher) listCurrent() (*model.KVPairList, error) {
	wc.Logger.Info("Performing initial list with no revision")
	list, err := wc.client.List(wc.Ctx, wc.list, "")
	if err != nil {
		return nil, err
	}

	wc.initialRev, err = strconv.ParseInt(list.Revision, 10, 64)
	if err != nil {
		wc.Logger.WithError(err).Error("List returned revision that could not be parsed")
		return nil, err
	}

	return list, nil
}

// removeDefaultAllowProfile filters out the default-allow profile out of the
// given kvps list.
func (wc *watcher) removeDefaultAllowProfile(list *model.KVPairList) {
	wc.Logger.Debugf("Filtering the default-allow profile out of the kvps list")
	n := 0
	s := list.KVPairs
	for _, kvp := range s {
		if kvp.Key != defaultAllowProfileResourceKey {
			s[n] = kvp
			n++
		}
	}
	list.KVPairs = s[:n]
}

// sendAddedEvents sends an ADDED event for each entry in the kvp list.
func (wc *watcher) sendAddedEvents(list *model.KVPairList) {
	for _, kv := range list.KVPairs {
		wc.sendEvent(&api.WatchEvent{
			Type: api.WatchAdded,
			New:  kv,
		})
	}
}

// terminateWatcher terminates the resources associated with the watcher.
func (wc *watcher) terminateWatcher() {
	wc.Logger.Debug("Terminating etcdv3 watcher")
	// Cancel the context - which will cancel the etcd Watch, this may have already been
	// cancelled through an explicit Stop, but it is fine to cancel multiple times.
	wc.Cancel()

	// Close the results channel.
	wc.CloseResultChan()

	// Increment the terminated counter using a goroutine safe operation.
	atomic.AddUint32(&wc.terminated, 1)
}

// sendError packages up the error as an event and sends it in the results channel.
func (wc *watcher) sendError(err error) {
	// The response from etcd commands may include a context.Canceled error if the context
	// was cancelled before completion.  Since with our Watcher we don't include that as
	// an error type skip over the Canceled error, the error processing in the main
	// watch thread will terminate the watcher.
	if err == context.Canceled {
		return
	}

	// Wrap the error up in a WatchEvent and use sendEvent to send it.
	errEvent := &api.WatchEvent{
		Type:  api.WatchError,
		Error: err,
	}
	wc.sendEvent(errEvent)
}

// sendEvent sends an event in the results channel.
func (wc *watcher) sendEvent(e *api.WatchEvent) {
	wc.SendEvent(*e)
}

// runListAndWatch contains the main list-watch loop for etcd clients.
func (wc *watcher) runListAndWatch() {
	wc.RunListAndWatchLoop(wc, wc.terminateWatcher)
}
