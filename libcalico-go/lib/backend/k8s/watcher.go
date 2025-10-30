package k8s

import (
	"context"
	"errors"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"

	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/resources"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/watchersyncer"
	cerrors "github.com/projectcalico/calico/libcalico-go/lib/errors"
)

// watcher implements the watch interface for Kubernetes resources.
type watcher struct {
	*watchersyncer.BaseWatcher
	client       resources.K8sResourceClient
	list         model.ListInterface
	crdInstalled bool
	watch        api.WatchInterface
}

// PerformList executes the list operation for K8s backend.
func (wc *watcher) PerformList(ctx context.Context, revision string) (*model.KVPairList, error) {
	result, err := wc.client.List(ctx, wc.list, revision)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// CRD not installed yet
			wc.Logger.Debug("Backing API not installed, marking as in-sync and retrying later.")
			wc.ResyncBlockedUntil = time.Now().Add(watchersyncer.MissingAPIRetryTime)
			wc.crdInstalled = false
			// Send InSync event for CRD not installed case
			wc.SendEvent(api.WatchEvent{Type: api.WatchInSync})
			return result, err
		}
		// If we get this far, we know the API is installed even if we got an error.
		wc.markInstalled()
		wc.Logger.WithError(err).Info("Failed to perform list of current data during resync")
		wc.ResyncBlockedUntil = time.Now().Add(watchersyncer.ListRetryInterval)
	}
	wc.markInstalled()
	return result, err
}

// StartWatch initiates the watch operation for K8s backend.
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
		Revision:            revision,
		AllowWatchBookmarks: true,
	})
}

// HandleWatchError processes watch errors for Kubernetes backend.
// It implements error-specific handling strategies:
//   - For revision expiration errors (ResourceExpired, Gone, TooLargeResourceVersion):
//     immediately triggers a full resync as the watch revision is too old
//   - For transient connectivity errors (ConnectionRefused, TooManyRequests):
//     logs and retries without resetting revision
//   - For unsupported operation errors: falls back to polling mode
//   - For other errors: tracks consecutive failures and triggers full resync
//     if MaxErrorsPerRevision threshold is exceeded
func (wc *watcher) HandleWatchError(err error) {
	// Check if we should reset revision based on the error type
	if kerrors.IsResourceExpired(err) || kerrors.IsGone(err) || isTooLargeResourceVersionError(err) {
		// Our current watch CurrentRevision is too old.  Even with watch bookmarks, we hit this path after the
		// API server restarts (and presumably does an immediate compaction).
		wc.Logger.WithError(err).Info("Watch has expired, triggering full resync.")
		wc.ResetRevisionForFullResync()
	} else {
		// Log specific K8s errors but don't reset revision
		if utilnet.IsConnectionRefused(err) || kerrors.IsTooManyRequests(err) {
			wc.Logger.WithError(err).Warn("API server refused connection, will retry.")
			return
		}

		var errNotSupp cerrors.ErrorOperationNotSupported
		var errNotExist cerrors.ErrorResourceDoesNotExist
		if errors.As(err, &errNotSupp) ||
			errors.As(err, &errNotExist) {
			// Watch is not supported on this resource type, either because the type fundamentally
			// doesn't support it, or because there are no resources to watch yet (and Kubernetes won't
			// let us watch if there are no resources yet). Pause for the watch poll interval.
			// This loop effectively becomes a poll loop for this resource type.
			wc.Logger.Debug("Watch operation not supported; reverting to poll.")
			wc.ResyncBlockedUntil = time.Now().Add(watchersyncer.WatchPollInterval)
			wc.ResetRevisionForFullResync()
			return
		}

		// Unknown error, default is to just try restarting the watch on assumption that it's
		// a connectivity issue.  Note that, if the error recurs when recreating the watch, we will
		// check for various expected connectivity failure conditions and handle them there.
		wc.ErrorCountAtCurrentRev++
		if wc.ErrorCountAtCurrentRev >= watchersyncer.MaxErrorsPerRevision {
			wc.Logger.Warn("Watch repeatedly failed without making progress, triggering full resync")
			wc.ResetRevisionForFullResync()
		}
		wc.Logger.Info("Watch of resource finished. Attempting to restart it...")
	}
}

// ListAndWatch performs list and watch operations for Kubernetes, handling
// reconnection, resync, and error recovery. This includes Kubernetes-specific
// logic for handling bookmarks and CRD installation detection.
func (c *KubeClient) ListAndWatch(ctx context.Context, l model.ListInterface) (<-chan api.WatchEvent, error) {
	log.Debugf("Performing 'ListAndWatch' for %+v %v", l, reflect.TypeOf(l))
	client := c.getResourceClientFromList(l)
	if client == nil {
		log.Debug("Attempt to 'ListAndWatch' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: l,
			Operation:  "ListAndWatch",
		}
	}

	wc := &watcher{
		BaseWatcher:  watchersyncer.NewBaseWatcher(ctx, log.WithField("list", l)),
		client:       client,
		list:         l,
		crdInstalled: true, // Assume true until we detect otherwise
	}
	go wc.runListAndWatch()

	return wc.ResultChan, nil
}

// terminateWatcher terminates the resources associated with the watcher.
func (wc *watcher) terminateWatcher() {
	wc.Logger.Debug("Terminating k8s watcher")
	wc.Cancel()
	if wc.watch != nil {
		wc.watch.Stop()
	}
	wc.CloseResultChan()
}

// markInstalled marks the CRD as installed.
func (wc *watcher) markInstalled() {
	if !wc.crdInstalled {
		wc.Logger.Info("Backing API has been installed")
		wc.crdInstalled = true
	}
}

// runListAndWatch contains the main list-watch loop for Kubernetes clients.
func (wc *watcher) runListAndWatch() {
	wc.RunListAndWatchLoop(wc, wc.terminateWatcher)
}
