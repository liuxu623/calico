// Copyright (c) 2017-2025 Tigera, Inc. All rights reserved.

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

package watchersyncer

import (
	"context"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
)

// The watcherCache provides watcher/syncer support for a single key type in the
// backend.  These results are sent to the main WatcherSyncer on a buffered "results"
// channel.  To ensure the order of events is received correctly by the main WatcherSyncer,
// we send all notification types in this channel.  Note that because of this the results
// channel is untyped - however the watcherSyncer only expects one of the following
// types:
// -  An error
// -  An api.Update
// -  A api.SyncStatus (only for the very first InSync notification)
type watcherCache struct {
	logger            *logrus.Entry
	client            api.Client
	resources         map[string]cacheEntry
	oldResources      map[string]cacheEntry
	results           chan<- interface{}
	hasSynced         bool
	resourceType      ResourceType
	watchRetryTimeout time.Duration
}

const (
	MaxErrorsPerRevision = 5
	resultsBufSize       = 100
	InitialRevision      = "0"
)

var (
	MinResyncInterval        = 500 * time.Millisecond
	ListRetryInterval        = 1000 * time.Millisecond
	WatchPollInterval        = 5000 * time.Millisecond
	DefaultWatchRetryTimeout = 600 * time.Second

	// If the backing API is not installed, we consider ourselves in-sync but retry
	// infrequently. If the API is eventually installed, we will resync after this timer pops.
	// However, it's good practice to restart Calico when installing a new API to expedite this.
	MissingAPIRetryTime = 30 * time.Minute
)

// cacheEntry is an entry in our cache.  It groups the a key with the last known
// revision that we processed.  We store the revision so that we can determine
// if an entry has been updated (and therefore whether we need to send an update
// event in the syncer callback).
type cacheEntry struct {
	revision string
	key      model.Key
}

// Create a new watcherCache.
func newWatcherCache(client api.Client, resourceType ResourceType, results chan<- interface{}) *watcherCache {
	return &watcherCache{
		logger:       logrus.WithField("ListRoot", listRootForLog(resourceType.ListInterface)),
		client:       client,
		resourceType: resourceType,
		results:      results,
		resources:    make(map[string]cacheEntry, 0),
	}
}

func listRootForLog(listInterface model.ListInterface) string {
	root := model.ListOptionsToDefaultPathRoot(listInterface)
	root = strings.Replace(root, "/calico/resources/v3/projectcalico.org/", ".../v3/pc.org/", 1)
	root = strings.Replace(root, "/calico/", ".../", 1)
	return root
}

// run creates the watcher using ListAndWatch and loops indefinitely reading from the event channel.
func (wc *watcherCache) run(ctx context.Context) {
	wc.logger.Debug("Watcher cache starting...")

	// On shutdown, send deletions for all the objects we're tracking.
	defer wc.sendDeletionsForAllResources()

	// Create the ListAndWatch event channel
	eventChan, err := wc.client.ListAndWatch(ctx, wc.resourceType.ListInterface)
	if err != nil {
		wc.logger.WithError(err).Error("Failed to create ListAndWatch")
		wc.results <- err
		return
	}

	// Read events from the channel
	wc.loopReadingFromEventChannel(ctx, eventChan)
}

func (wc *watcherCache) loopReadingFromEventChannel(ctx context.Context, eventChan <-chan api.WatchEvent) {
	eventLogger := wc.logger.WithField("event", nil)

	for {
		select {
		case <-ctx.Done():
			wc.logger.Debug("Context is done. Returning")
			return
		case event, ok := <-eventChan:
			if !ok {
				// If the channel is closed, we're done
				wc.logger.Debug("Event channel closed")
				return
			}

			// Re-use this log event so that we don't allocate every time.
			eventLogger.Data["event"] = event
			eventLogger.Debug("Got event from event channel")

			// Handle the specific event type.
			switch event.Type {
			case api.WatchAdded, api.WatchModified:
				kvp := event.New
				wc.handleWatchListEvent(kvp)
			case api.WatchDeleted:
				// Nil out the value to indicate a delete.
				kvp := event.Old
				if kvp == nil {
					// Bug, we're about to panic when we hit the nil pointer, log something useful.
					eventLogger.Panic("Deletion event without old value")
				}
				kvp.Value = nil
				wc.handleWatchListEvent(kvp)
			case api.WatchInSync:
				wc.finishResync()
			default:
				// Unknown event type - not much we can do other than log.
				eventLogger.Errorf("Unknown event type received from the datastore: %v", event.Type)
			}
		}
	}
}

// finishResync handles processing to finish synchronization.
// If this watcher has never been synced then notify the main watcherSyncer that we've synced.
// We may also need to send deleted messages for old resources that were not validated in the
// resync (i.e. they must have since been deleted).
func (wc *watcherCache) finishResync() {
	// If we haven't already sent an InSync event then send a synced notification.  The watcherSyncer will send a Synced
	// event when it has received synced events from each cache. Once in-sync the cache remains in-sync.
	if !wc.hasSynced {
		wc.logger.Info("Sending synced update")
		wc.results <- api.InSync
		wc.hasSynced = true
	}

	// If the watcher failed at any time, we end up recreating a watcher and storing off
	// the current known resources for revalidation.  Now that we have finished the sync,
	// any of the remaining resources that were not accounted for must have been deleted
	// and we need to send deleted events for them.
	numOldResources := len(wc.oldResources)
	if numOldResources > 0 {
		wc.logger.WithField("Num", numOldResources).Debug("Sending resync deletes")
		updates := make([]api.Update, 0, len(wc.oldResources))
		for _, r := range wc.oldResources {
			updates = append(updates, api.Update{
				UpdateType: api.UpdateTypeKVDeleted,
				KVPair: model.KVPair{
					Key: r.key,
				},
			})
		}
		wc.results <- updates
	}
	wc.oldResources = nil
}

// handleWatchListEvent handles a watch event converting it if required and passing to
// handleConvertedWatchEvent to send the appropriate update types.
func (wc *watcherCache) handleWatchListEvent(kvp *model.KVPair) {
	if wc.resourceType.UpdateProcessor == nil {
		// No update processor - handle immediately.
		wc.handleConvertedWatchEvent(kvp)
		return
	}

	// We have an update processor so use that to convert the event data.
	kvps, err := wc.resourceType.UpdateProcessor.Process(kvp)
	for _, kvp := range kvps {
		wc.handleConvertedWatchEvent(kvp)
	}

	// If we hit a conversion error, log the error and notify the main syncer.
	if err != nil {
		wc.results <- err
	}
}

// handleConvertedWatchEvent handles a converted watch event fanning out
// to the add/mod or delete processing as necessary.
func (wc *watcherCache) handleConvertedWatchEvent(kvp *model.KVPair) {
	if kvp.Value == nil {
		wc.handleDeletedUpdate(kvp.Key)
	} else {
		wc.handleAddedOrModifiedUpdate(kvp)
	}
}

// handleAddedOrModifiedUpdate handles a single Added or Modified update request.
// Whether we send an Added or Modified depends on whether we have already sent
// an added notification for this resource.
func (wc *watcherCache) handleAddedOrModifiedUpdate(kvp *model.KVPair) {
	thisKey := kvp.Key
	thisKeyString := thisKey.String()
	thisRevision := kvp.Revision
	wc.markAsValid(thisKeyString)

	// If the resource is already in our map, then this is a modified event.  Check the
	// revision to see if we actually need to send an update.
	if resource, ok := wc.resources[thisKeyString]; ok {
		if resource.revision == thisRevision {
			// No update to revision, so no event to send.
			wc.logger.WithField("Key", thisKeyString).Debug("Swallowing event update from datastore because entry is same as cached entry")
			return
		}
		// Resource is modified, send an update event and store the latest revision.
		wc.logger.WithField("Key", thisKeyString).Debug("Datastore entry modified, sending syncer update")
		wc.results <- []api.Update{{
			UpdateType: api.UpdateTypeKVUpdated,
			KVPair:     *kvp,
		}}
		resource.revision = thisRevision
		wc.resources[thisKeyString] = resource
		return
	}

	// The resource has not been seen before, so send a new event, and store the
	// current revision.
	wc.logger.WithField("Key", thisKeyString).Debug("Cache entry added, sending syncer update")
	wc.results <- []api.Update{{
		UpdateType: api.UpdateTypeKVNew,
		KVPair:     *kvp,
	}}
	wc.resources[thisKeyString] = cacheEntry{
		revision: thisRevision,
		key:      thisKey,
	}
}

// handleDeletedWatchEvent sends a deleted event and removes the resource key from our cache.
func (wc *watcherCache) handleDeletedUpdate(key model.Key) {
	thisKeyString := key.String()
	wc.markAsValid(thisKeyString)

	// If we have seen an added event for this key then send a deleted event and remove
	// from the cache.
	if _, ok := wc.resources[thisKeyString]; ok {
		wc.logger.WithField("Key", thisKeyString).Debug("Datastore entry deleted, sending syncer update")
		wc.results <- []api.Update{{
			UpdateType: api.UpdateTypeKVDeleted,
			KVPair: model.KVPair{
				Key: key,
			},
		}}
		delete(wc.resources, thisKeyString)
	}
}

// markAsValid marks a resource that we have just seen as valid, by moving it from the set of
// "oldResources" that were stored during the resync back into the main "resources" set.  Any entries
// remaining in the oldResources map once the current snapshot events have been processed, indicates
// entries that were deleted during the resync - see corresponding code in finishResync().
func (wc *watcherCache) markAsValid(resourceKey string) {
	if wc.oldResources != nil {
		if oldResource, ok := wc.oldResources[resourceKey]; ok {
			wc.logger.WithField("Key", resourceKey).Debug("Marking key as re-processed")
			wc.resources[resourceKey] = oldResource
			delete(wc.oldResources, resourceKey)
		}
	}
}

func (wc *watcherCache) sendDeletionsForAllResources() {
	for _, value := range wc.resources {
		wc.results <- []api.Update{{
			UpdateType: api.UpdateTypeKVDeleted,
			KVPair: model.KVPair{
				Key: value.key,
			},
		}}
	}
	clear(wc.resources)
}

// ClosedTimeC is a pre-closed channel used to trigger immediate execution.
// This avoids allocating a new channel every time we need an immediate trigger.
var ClosedTimeC = make(chan time.Time)

func init() {
	close(ClosedTimeC)
}

// BaseWatcher contains common fields and methods shared across different watcher implementations.
type BaseWatcher struct {
	Logger                 *logrus.Entry
	Ctx                    context.Context
	Cancel                 context.CancelFunc
	ResultChan             chan api.WatchEvent
	CurrentRevision        string
	ErrorCountAtCurrentRev int
	ResyncBlockedUntil     time.Time
}

// NewBaseWatcher creates a new BaseWatcher with the given context and logger.
func NewBaseWatcher(ctx context.Context, logger *logrus.Entry) *BaseWatcher {
	bw := &BaseWatcher{
		Logger:          logger,
		ResultChan:      make(chan api.WatchEvent, resultsBufSize),
		CurrentRevision: "0",
	}
	bw.Ctx, bw.Cancel = context.WithCancel(ctx)
	return bw
}

// CreateWatchFromBackend is a helper method that creates a watch using the backend's
// CreateWatchWithErrorHandling. This reduces code duplication in backend implementations.
func (bw *BaseWatcher) CreateWatchFromBackend(backend WatcherBackend, list model.ListInterface) api.WatchInterface {
	return bw.CreateWatchWithErrorHandling(backend, list)
}

// ResetRevisionForFullResync resets the current revision and error count to trigger a full resync.
func (bw *BaseWatcher) ResetRevisionForFullResync() {
	bw.CurrentRevision = InitialRevision
	bw.ErrorCountAtCurrentRev = 0
}

// ResyncThrottleC returns a channel that blocks until resync is allowed.
func (bw *BaseWatcher) ResyncThrottleC() <-chan time.Time {
	blockFor := time.Until(bw.ResyncBlockedUntil)
	var blockC <-chan time.Time
	if blockFor > 0 {
		bw.Logger.WithField("delay", blockFor).Debug("Sleeping before next resync")
		blockC = time.After(blockFor)
	} else {
		blockC = ClosedTimeC // Triggers immediately.
	}
	return blockC
}

// SendEvent sends an event to the result channel.
func (bw *BaseWatcher) SendEvent(event api.WatchEvent) {
	if len(bw.ResultChan) == resultsBufSize {
		bw.Logger.Warningf("Watch events backing up: %d events", resultsBufSize)
	}
	select {
	case bw.ResultChan <- event:
	case <-bw.Ctx.Done():
	}
}

// CloseResultChan closes the result channel.
func (bw *BaseWatcher) CloseResultChan() {
	close(bw.ResultChan)
}

// SendListResultsAsAddedEvents sends all KVPairs in the list as ADDED events.
func (bw *BaseWatcher) SendListResultsAsAddedEvents(kvPairs []*model.KVPair) {
	for _, kvp := range kvPairs {
		bw.SendEvent(api.WatchEvent{Type: api.WatchAdded, New: kvp})
	}
}

// SendInSyncEvent sends an InSync event.
func (bw *BaseWatcher) SendInSyncEvent() {
	bw.SendEvent(api.WatchEvent{Type: api.WatchInSync})
}

// UpdateRevisionFromEvent updates the current revision from a watch event and resets error count.
// Returns true if the revision was updated.
func (bw *BaseWatcher) UpdateRevisionFromEvent(event *api.WatchEvent) bool {
	var updated bool
	if event.New != nil {
		bw.CurrentRevision = event.New.Revision
		updated = true
	} else if event.Old != nil {
		bw.CurrentRevision = event.Old.Revision
		updated = true
	}

	if updated {
		bw.ErrorCountAtCurrentRev = 0
	}
	return updated
}

// WatcherBackend defines the interface for backend-specific operations.
// Implementations must provide these methods for the template method pattern.
type WatcherBackend interface {
	// PerformList executes the list operation from the specified revision and returns the result.
	// The implementation should handle backend-specific error cases such as:
	// - Connection failures
	// - API not installed (for K8s CRDs)
	// - Permission errors
	// Returns the list result and any error encountered.
	PerformList(ctx context.Context, revision string) (*model.KVPairList, error)

	// StartWatch initiates the watch operation after a successful list.
	// This method should create the watch if needed and start reading from it.
	// Returns true if the watch loop should continue processing events,
	// or false if the outer loop should restart (e.g., watch creation failed).
	StartWatch() bool

	// GetWatch returns the current watch interface, or nil if no watch is active.
	// This is used by LoopReadingFromWatcher to access the watch's result channel.
	GetWatch() api.WatchInterface

	// SetWatch sets the watch interface. Used internally by StartWatchImpl.
	SetWatch(w api.WatchInterface)

	// CreateWatch creates a watch from the specified revision with backend-specific options.
	// The revision parameter indicates where to start watching from.
	// Backend implementations may add specific watch options (e.g., bookmarks for K8s).
	// Returns the watch interface and any error encountered during creation.
	CreateWatch(ctx context.Context, list model.ListInterface, revision string) (api.WatchInterface, error)

	HandleWatchError(err error)
}

// StartWatchImpl provides the common implementation for StartWatch.
// Backend implementations can call this from their StartWatch method.
func (bw *BaseWatcher) StartWatchImpl(backend WatcherBackend, list model.ListInterface) bool {
	if backend.GetWatch() == nil {
		// Create watch using the backend's CreateWatch method
		w := bw.CreateWatchFromBackend(backend, list)
		if w == nil {
			bw.Logger.Debug("Failed to create watch, will retry")
			return false
		}
		backend.SetWatch(w)
	}

	bw.Logger.Debug("Resync completed, now watching for change events")
	bw.LoopReadingFromWatcher(backend)
	return true
}

// RunListAndWatchLoop executes the common list-and-watch loop logic.
// This is a template method that delegates backend-specific operations to the WatcherBackend interface.
func (bw *BaseWatcher) RunListAndWatchLoop(backend WatcherBackend, terminateFunc func()) {
	defer terminateFunc()

	for {
		start := time.Now()
		select {
		case <-bw.Ctx.Done():
			bw.Logger.Debug("Context is done. Returning")
			return
		case <-bw.ResyncThrottleC():
			bw.Logger.Debugf("Starting main resync loop after delay %v", time.Since(start))
		}

		// Avoid tight loop in unexpected failure scenarios
		bw.ResyncBlockedUntil = time.Now().Add(MinResyncInterval)

		if bw.CurrentRevision == InitialRevision {
			bw.Logger.Info("Full resync is required")

			listResult, err := backend.PerformList(bw.Ctx, bw.CurrentRevision)
			if err != nil {
				continue
			}

			if listResult != nil {
				bw.Logger.WithField("revision", listResult.Revision).Debug("List completed.")

				if listResult.Revision == "" || listResult.Revision == InitialRevision {
					if len(listResult.KVPairs) == 0 {
						bw.Logger.Info("List returned no items and an empty/zero revision, reverting to poll.")
						bw.ResyncBlockedUntil = time.Now().Add(WatchPollInterval)
						continue
					}
					bw.Logger.Panic("BUG: List returned items with empty/zero revision. Watch would be inconsistent.")
				}
				bw.CurrentRevision = listResult.Revision

				// Send list results as ADDED events and InSync event
				bw.SendListResultsAsAddedEvents(listResult.KVPairs)
				bw.SendInSyncEvent()
			}
		}

		// Start watching for changes
		if !backend.StartWatch() {
			continue
		}
	}
}

// CreateWatchWithErrorHandling is a common implementation for creating watches with error handling.
// It handles connection errors and retry logic.
func (bw *BaseWatcher) CreateWatchWithErrorHandling(
	backend WatcherBackend,
	list model.ListInterface,
) api.WatchInterface {
	bw.Logger.WithField("revision", bw.CurrentRevision).Debug("Starting watch from revision")

	w, err := backend.CreateWatch(bw.Ctx, list, bw.CurrentRevision)
	if err != nil {
		bw.ErrorCountAtCurrentRev++
		bw.Logger.WithError(err).WithField("errorsWithoutProgress", bw.ErrorCountAtCurrentRev).Warn("Failed to create watcher; will retry.")
		if bw.ErrorCountAtCurrentRev >= MaxErrorsPerRevision {
			bw.ResetRevisionForFullResync()
		}
		bw.ResyncBlockedUntil = time.Now().Add(WatchPollInterval)
		return nil
	}

	return w
}

// LoopReadingFromWatcher reads events from the watch interface and processes them.
// This is the common implementation extracted from both k8s and etcdv3 backends.
// It returns when the watch needs to be recreated (e.g., channel closed or error).
func (bw *BaseWatcher) LoopReadingFromWatcher(backend WatcherBackend) {
	watch := backend.GetWatch()
	if watch == nil {
		bw.Logger.Warn("Watch is nil, cannot loop reading from watcher")
		return
	}

	eventLogger := bw.Logger.WithField("event", nil)

	for {
		select {
		case <-bw.Ctx.Done():
			bw.Logger.Debug("Context is done. Returning")
			return
		case event, ok := <-watch.ResultChan():
			if !ok {
				// If the channel is closed then resync/recreate the watch.
				bw.Logger.Debug("Watch channel closed by remote - recreate watcher")
				return
			}

			// Re-use this log event so that we don't allocate every time.
			eventLogger.Data["event"] = event
			eventLogger.Debug("Got event from results channel")

			// Handle the specific event type.
			switch event.Type {
			case api.WatchAdded, api.WatchModified, api.WatchDeleted:
				// Update CurrentRevision from events
				bw.UpdateRevisionFromEvent(&event)
				bw.SendEvent(event)
			case api.WatchBookmark:
				bw.Logger.WithField("newRevision", event.New.Revision).Debug("Watch bookmark received")
				bw.UpdateRevisionFromEvent(&event)
			case api.WatchError:
				backend.HandleWatchError(event.Error)
				return
			default:
				// Unknown event type - not much we can do other than log.
				eventLogger.Errorf("Unknown event type received from the datastore")
			}
		}
	}
}
