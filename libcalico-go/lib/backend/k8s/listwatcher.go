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

package k8s

import (
	"context"
	"errors"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/utils/pointer"

	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/resources"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
	cerrors "github.com/projectcalico/calico/libcalico-go/lib/errors"
)

// k8sListWatcher implements the ListAndWatch logic for Kubernetes backends.
// It encapsulates all k8s-specific logic for list-watch operations, including:
// - CRD installation detection
// - Bookmark handling
// - WatchList support with fallback
// - Error handling and retry logic
type k8sListWatcher struct {
	client       *KubeClient
	list         model.ListInterface
	options      api.WatchOptions
	handler      api.EventHandler
	crdInstalled bool

	// Configuration for retry and timing
	minResyncInterval   time.Duration
	listRetryInterval   time.Duration
	watchPollInterval   time.Duration
	missingAPIRetryTime time.Duration
	watchRetryTimeout   time.Duration

	// State tracking
	currentRevision        string
	errorCountAtCurrentRev int
	lastSuccessfulConnTime time.Time
	fallbackToList         bool
}

// newK8sListWatcher creates a new k8sListWatcher instance
func newK8sListWatcher(client *KubeClient, list model.ListInterface, options api.WatchOptions, handler api.EventHandler) *k8sListWatcher {
	return &k8sListWatcher{
		client:                 client,
		list:                   list,
		options:                options,
		handler:                handler,
		crdInstalled:           true, // Assume true until we detect otherwise
		minResyncInterval:      500 * time.Millisecond,
		listRetryInterval:      1000 * time.Millisecond,
		watchPollInterval:      5000 * time.Millisecond,
		missingAPIRetryTime:    30 * time.Minute,
		watchRetryTimeout:      600 * time.Second,
		currentRevision:        options.Revision,
		lastSuccessfulConnTime: time.Now(),
	}
}

// run executes the list-and-watch loop
func (w *k8sListWatcher) run(ctx context.Context) error {
	log.Debug("Starting k8s ListAndWatch")

	for {
		select {
		case <-ctx.Done():
			log.Debug("Context cancelled, stopping ListAndWatch")
			return ctx.Err()
		default:
			// Continue with list/watch cycle
		}

		// Perform list or list-watch operation
		if err := w.listAndWatchCycle(ctx); err != nil {
			// Check if the error is recoverable
			if errors.Is(err, context.Canceled) {
				return err
			}

			// Handler recovered from the error, continue after a short delay
			time.Sleep(w.minResyncInterval)
		}
	}
}

// listAndWatchCycle performs one cycle of list (if needed) and watch
func (w *k8sListWatcher) listAndWatchCycle(ctx context.Context) error {
	// Get the resource client
	client := w.client.getResourceClientFromList(w.list)
	if client == nil {
		log.Debug("Watch operation not supported for this resource type")
		return cerrors.ErrorOperationNotSupported{
			Identifier: w.list,
			Operation:  "Watch",
		}
	}

	// Determine if we need to perform a full list or can use WatchList
	needsFullList := w.currentRevision == "" || w.fallbackToList

	if needsFullList {
		log.Info("Full resync is required")

		// Perform the list operation
		list, err := w.client.List(ctx, w.list, w.currentRevision)
		if err != nil {
			return w.handleListError(err)
		}

		// Successfully listed resources
		w.lastSuccessfulConnTime = time.Now()
		w.markInstalled()

		// Send add events for each resource
		for _, kvp := range list.KVPairs {
			w.handler.OnAdd(kvp)
		}

		// Update current revision to the list revision
		w.currentRevision = list.Revision
		w.errorCountAtCurrentRev = 0

		w.handler.OnSync()
	}

	// Set up watch options for continuous watching
	watchOptions := api.WatchOptions{
		Revision:            w.currentRevision,
		AllowWatchBookmarks: true,
	}

	// If we're at revision "0", this indicates we want to use WatchList
	if w.currentRevision == "0" {
		watchOptions.SendInitialEvents = pointer.Bool(true)
		watchOptions.ResourceVersionMatch = metav1.ResourceVersionMatchNotOlderThan
	}

	// Start watching from the current revision
	watch, err := client.Watch(ctx, w.list, watchOptions)
	if err != nil {
		return w.handleWatchError(err)
	}
	defer watch.Stop()

	w.markInstalled()
	log.Debug("Starting watch from revision:", w.currentRevision)

	// Process watch events
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-watch.ResultChan():
			if !ok {
				log.Debug("Watch channel closed by remote")
				return errors.New("watch channel closed")
			}

			if err := w.handleWatchEvent(ctx, event); err != nil {
				return err
			}
		}
	}
}

// handleListError processes errors from the List operation
func (w *k8sListWatcher) handleListError(err error) error {
	if kerrors.IsNotFound(err) {
		// The resource type doesn't exist yet (CRD not installed)
		// This is a valid long-term state, so we don't want to keep retrying rapidly
		log.Info("Backing API not installed, marking as in-sync and retrying later")

		w.crdInstalled = false

		// Notify handler that we're in sync (even though API is not installed)
		// This allows the syncer to proceed without this resource type
		w.handler.OnSync()

		// Sleep for a long time before retrying
		time.Sleep(w.missingAPIRetryTime)
		return nil // Return nil to continue the loop
	}

	// If we get this far, we know the API is installed even if we got an error
	w.markInstalled()

	log.WithError(err).Info("Failed to perform list of current data")

	if kerrors.IsResourceExpired(err) || isTooLargeResourceVersionError(err) {
		// Our current watch revision is out of sync, start again without a revision
		log.Info("Resource too old/new error from server, clearing cached watch revision")
		w.currentRevision = ""
		w.errorCountAtCurrentRev = 0
		// Error is a "layer 7" error; so connection is good!
		w.lastSuccessfulConnTime = time.Now()
	} else if time.Since(w.lastSuccessfulConnTime) > w.watchRetryTimeout {
		// Connection to datastore has failed for too long
		log.Warn("Connection to datastore has failed - signaling error to client")
		return err
	}

	// Sleep before retrying
	time.Sleep(w.listRetryInterval)
	return nil // Return nil to continue the loop
}

// handleWatchError processes errors from the Watch operation
func (w *k8sListWatcher) handleWatchError(err error) error {
	if kerrors.IsNotFound(err) {
		// The resource type doesn't exist yet (CRD not installed)
		log.Info("Backing API not installed, marking as in-sync and retrying later")

		w.crdInstalled = false

		// Notify handler that we're in sync
		w.handler.OnSync()

		// Sleep for a long time before retrying
		time.Sleep(w.missingAPIRetryTime)
		return nil
	}

	// If we get this far, we know the API is installed even if we got an error
	w.markInstalled()

	// Check if WatchList is not supported
	if w.currentRevision == "0" && kerrors.IsInvalid(err) {
		log.WithError(err).Warn("Backend not support WatchList, falling back to List")
		w.fallbackToList = true
		return nil
	}

	// Check for revision-related errors
	if kerrors.IsResourceExpired(err) || kerrors.IsGone(err) || isTooLargeResourceVersionError(err) {
		// Our current watch revision is too old (or too new!), start again
		log.Info("Watch has expired, queueing full resync")
		w.currentRevision = ""
		w.errorCountAtCurrentRev = 0
		// Error is a "layer 7" error; so connection is good!
		w.lastSuccessfulConnTime = time.Now()
		return nil
	}

	// Check for connection-related errors
	if utilnet.IsConnectionRefused(err) || kerrors.IsTooManyRequests(err) {
		// Connection-related error, we can just retry without resetting the watch
		if time.Since(w.lastSuccessfulConnTime) > w.watchRetryTimeout {
			// Too long since we were connected
			log.WithError(err).Warn("Timed out waiting for connection to be restored, forcing resync")
			w.currentRevision = ""
			return nil
		}
		log.WithError(err).Warn("API server refused connection, will retry")
		return nil
	}

	// Check if watch is not supported
	opErr := cerrors.ErrorOperationNotSupported{}
	notExistErr := cerrors.ErrorResourceDoesNotExist{}
	if errors.As(err, &opErr) || errors.As(err, &notExistErr) {
		// Watch is not supported on this resource type
		log.Debug("Watch operation not supported; reverting to poll")

		// Force a re-list when we retry
		w.currentRevision = ""

		// Sleep for the poll interval
		time.Sleep(w.watchPollInterval)
		return nil
	}

	// None of our expected errors, retry a few times before we give up
	w.errorCountAtCurrentRev++
	maxErrorsPerRevision := 5
	if w.errorCountAtCurrentRev >= maxErrorsPerRevision {
		// Too many errors at the current revision, trigger a full resync
		log.WithError(err).Warn("Watch repeatedly failed without making progress, triggering full resync")
		w.currentRevision = ""
		return nil
	}

	log.WithError(err).Warn("Watch of resource finished. Attempting to restart it...")
	return nil
}

// handleWatchEvent processes a single watch event
func (w *k8sListWatcher) handleWatchEvent(ctx context.Context, event api.WatchEvent) error {
	switch event.Type {
	case api.WatchAdded, api.WatchModified:
		kvp := event.New
		w.updateRevision(kvp.Revision)
		w.handler.OnUpdate(kvp)
		w.lastSuccessfulConnTime = time.Now()

	case api.WatchDeleted:
		kvp := event.Old
		if kvp == nil {
			log.Panic("Deletion event without old value")
		}
		w.updateRevision(kvp.Revision)
		w.handler.OnDelete(kvp)
		w.lastSuccessfulConnTime = time.Now()

	case api.WatchBookmark:
		w.handleBookmark(event)
		w.lastSuccessfulConnTime = time.Now()

	case api.WatchError:
		return w.handleWatchError(event.Error)

	default:
		log.Errorf("Unknown event type received from the datastore: %v", event.Type)
	}

	return nil
}

// handleBookmark processes a bookmark event
func (w *k8sListWatcher) handleBookmark(event api.WatchEvent) {
	log.WithField("newRevision", event.New.Revision).Debug("Watch bookmark received")
	w.updateRevision(event.New.Revision)

	// Check if this bookmark indicates sync completion (for WatchList)
	k8sRes, ok := event.New.Value.(resources.Resource)
	if ok && k8sRes.GetObjectMeta().GetAnnotations()[metav1.InitialEventsAnnotationKey] == "true" {
		// This bookmark indicates we've received all initial events
		w.handler.OnSync()
	}
}

// updateRevision updates the current revision and resets error count
func (w *k8sListWatcher) updateRevision(revision string) {
	if revision != "" {
		w.currentRevision = revision
		w.errorCountAtCurrentRev = 0
	}
}

// markInstalled marks the CRD as installed
func (w *k8sListWatcher) markInstalled() {
	if !w.crdInstalled {
		log.Info("Backing API has been installed")
		w.crdInstalled = true
	}
}

// This function mirrors the one in watchercache.go
func isTooLargeResourceVersionError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Too large resource version")
}
