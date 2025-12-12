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

	log "github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
)

const (
	resultsBufSize = 100
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
		client:     c,
		list:       l,
		initialRev: rev,
		resultChan: make(chan api.WatchEvent, resultsBufSize),
	}
	wc.ctx, wc.cancel = context.WithCancel(cxt)
	go wc.watchLoop()
	return wc, nil
}

// ListAndWatch implements the list-and-watch pattern for etcd resources.
// For etcd, this uses the existing watcher implementation which already handles
// both list and watch operations internally. Unlike kubernetes, etcd doesn't need
// separate handling for bookmarks, CRD installation, or WatchList fallbacks.
func (c *etcdV3Client) ListAndWatch(ctx context.Context, l model.ListInterface, options api.WatchOptions, handler api.EventHandler) error {
	log.Debug("Starting etcd ListAndWatch")

	var watcher api.WatchInterface
	currentRevision := "0"

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if currentRevision == "0" {
			kvps, err := c.List(ctx, l, "")
			if err != nil {
				continue
			}
			// Send updates for each of the resources we listed - this will revalidate entries in
			// the oldResources map.
			for _, kvp := range kvps.KVPairs {
				handler.OnAdd(kvp)
			}
			handler.OnSync()

			currentRevision = kvps.Revision

			watcher, err = c.Watch(ctx, l, api.WatchOptions{
				Revision: currentRevision,
			})
			if err != nil {
				continue
			}
		}

		// Process events from the watcher
		log.Debug("Processing watcher events")
		for {
			select {
			case <-ctx.Done():
				log.Debug("Context cancelled, stopping ListAndWatch")
				return ctx.Err()

			case event, ok := <-watcher.ResultChan():
				if !ok {
					log.Debug("Watch channel closed")
					return nil
				}

				// Handle the event based on type
				switch event.Type {
				case api.WatchAdded:
					// For etcd, WatchAdded is used for initial list results
					// and for actual added events during watch
					if event.New != nil {
						handler.OnAdd(event.New)
					}

				case api.WatchModified:
					if event.New != nil {
						handler.OnUpdate(event.New)
					}

				case api.WatchDeleted:
					if event.Old != nil {
						handler.OnDelete(event.Old)
					}

				case api.WatchError:
					if event.Error != nil {
						return event.Error
					}

				default:
					log.WithField("eventType", event.Type).Warn("Unexpected watch event type")
				}
			}
		}
	}
}

// watcher implements watch.Interface.
type watcher struct {
	client     *etcdV3Client
	initialRev int64
	ctx        context.Context
	cancel     context.CancelFunc
	resultChan chan api.WatchEvent
	list       model.ListInterface
	terminated uint32
}

// Stop stops the watcher and releases associated resources.
// This calls through to the context cancel function.
func (wc *watcher) Stop() {
	wc.cancel()
}

// ResultChan returns a channel used to receive WatchEvents.
func (wc *watcher) ResultChan() <-chan api.WatchEvent {
	return wc.resultChan
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
	logCxt := log.WithField("list", wc.list)
	key, opts := calculateListKeyAndOptions(logCxt, wc.list)

	log.Debug("Starting watcher.watchLoop")

	opts = append(opts, clientv3.WithRev(wc.initialRev+1), clientv3.WithPrevKV())
	logCxt = logCxt.WithFields(log.Fields{
		"etcdv3-etcdKey": key,
		"rev":            wc.initialRev,
	})
	logCxt.Debug("Starting etcdv3 watch")
	wch := wc.client.etcdClient.Watch(wc.ctx, key, opts...)
	for wres := range wch {
		if wres.Err() != nil {
			// A watch channel error is a terminating event, so exit the loop.
			err := wres.Err()
			log.WithError(err).Warning("Watch channel error")
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
	log.Warn("etcdv3 watch channel closed")
}

// listCurrent retrieves the existing entries.
func (wc *watcher) listCurrent() (*model.KVPairList, error) {
	log.Info("Performing initial list with no revision")
	list, err := wc.client.List(wc.ctx, wc.list, "")
	if err != nil {
		return nil, err
	}

	wc.initialRev, err = strconv.ParseInt(list.Revision, 10, 64)
	if err != nil {
		log.WithError(err).Error("List returned revision that could not be parsed")
		return nil, err
	}

	return list, nil
}

// terminateWatcher terminates the resources associated with the watcher.
func (wc *watcher) terminateWatcher() {
	log.Debug("Terminating etcdv3 watcher")
	// Cancel the context - which will cancel the etcd Watch, this may have already been
	// cancelled through an explicit Stop, but it is fine to cancel multiple times.
	wc.cancel()

	// Close the results channel.
	close(wc.resultChan)

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
	if len(wc.resultChan) == resultsBufSize {
		log.Warningf("Watch events backing up: %d events", resultsBufSize)
	}
	select {
	case wc.resultChan <- *e:
	case <-wc.ctx.Done():
	}
}
