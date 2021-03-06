// Copyright 2019 Antrea Authors
//
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

package ram

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog"

	"github.com/vmware-tanzu/antrea/pkg/apiserver/storage"
)

// storeWatcher implements watch.Interface
type storeWatcher struct {
	// input represents the channel for incoming internal events that should be processed.
	input chan storage.InternalEvent
	// result represents the channel for outgoing events that will be sent to the client.
	result chan watch.Event
	done   chan struct{}
	// selectors represent a watcher's conditions to select objects.
	selectors *storage.Selectors
	// forget is used to cleanup the watcher.
	forget func()
	// stopOnce guarantees Stop function will perform exactly once.
	stopOnce sync.Once
}

func newStoreWatcher(chanSize int, selectors *storage.Selectors, forget func()) *storeWatcher {
	return &storeWatcher{
		input:     make(chan storage.InternalEvent, chanSize),
		result:    make(chan watch.Event, chanSize),
		done:      make(chan struct{}),
		selectors: selectors,
		forget:    forget,
	}
}

// nonBlockingAdd tries to send event to channel input without blocking.
// It returns true if successful, otherwise false.
func (w *storeWatcher) nonBlockingAdd(event storage.InternalEvent) bool {
	select {
	case w.input <- event:
		return true
	default:
		return false
	}
}

// add tries to send event to channel input. It will first use non blocking
// way, then block until the provided timer fires, if the timer is not nil.
// It returns true if successful, otherwise false.
func (w *storeWatcher) add(event storage.InternalEvent, timer *time.Timer) bool {
	// Try to send the event without blocking regardless of timer is fired or not.
	// This gives the watcher a chance when other watchers exhaust the time slices.
	if w.nonBlockingAdd(event) {
		return true
	}

	if timer == nil {
		return false
	}

	select {
	case w.input <- event:
		return true
	case <-timer.C:
		return false
	}
}

// process first sends initEvents and then keeps sending events got from channel input
// if they are newer than the specified resourceVersion.
func (w *storeWatcher) process(ctx context.Context, initEvents []storage.InternalEvent, resourceVersion uint64) {
	for _, event := range initEvents {
		w.sendWatchEvent(event)
	}
	defer close(w.result)
	for {
		select {
		case event, ok := <-w.input:
			if !ok {
				klog.Info("The input channel had been closed, stopping process")
				return
			}
			if event.GetResourceVersion() > resourceVersion {
				w.sendWatchEvent(event)
			}
		case <-ctx.Done():
			klog.Info("The context had been canceled, stopping process")
			return
		}
	}
}

// sendWatchEvent converts an InternalEvent to watch.Event based on the watcher's selectors.
// It sends the converted event to result channel, if not nil.
func (w *storeWatcher) sendWatchEvent(event storage.InternalEvent) {
	watchEvent := event.ToWatchEvent(w.selectors)
	if watchEvent == nil {
		// Watcher is not interested in that object.
		return
	}

	select {
	case <-w.done:
		return
	default:
	}

	select {
	case w.result <- *watchEvent:
	case <-w.done:
	}
}

// ResultChan returns the channel for outgoing events to the client.
func (w *storeWatcher) ResultChan() <-chan watch.Event {
	return w.result
}

// Stop stops this watcher.
// It must be idempotent and thread safe as it could be called by apiserver endpoint handler
// and dispatchEvent concurrently.
func (w *storeWatcher) Stop() {
	w.stopOnce.Do(func() {
		w.forget()
		close(w.done)
		// forget removes this watcher from the store's watcher list, there won't
		// be events sent to its input channel so we are safe to close it.
		close(w.input)
	})
}
