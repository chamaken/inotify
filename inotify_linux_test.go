// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package inotify

import (
	"io/ioutil"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestInotifyEvents(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}

	dir, err := ioutil.TempDir("", "inotify")
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Add a watch for "_test"
	err = watcher.Watch(dir, nil)
	if err != nil {
		t.Fatalf("Watch failed: %s", err)
	}

	// Receive errors on the error channel on a separate goroutine
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()

	testFile := dir + "/TestInotifyEvents.testfile"

	// Receive events on the event channel on a separate goroutine
	eventstream := watcher.Event
	var eventsReceived int32 = 0
	done := make(chan bool)
	go func() {
		for event := range eventstream {
			// Only count relevant events
			if event.Name == testFile {
				atomic.AddInt32(&eventsReceived, 1)
				t.Logf("event received: %s", event)
			} else {
				t.Logf("unexpected event received: %s", event)
			}
		}
		done <- true
	}()

	// Create a file
	// This should add at least one event to the inotify event queue
	_, err = os.OpenFile(testFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("creating test file: %s", err)
	}

	// We expect this event to be received almost immediately, but let's wait 1 s to be sure
	time.Sleep(1 * time.Second)
	if atomic.AddInt32(&eventsReceived, 0) == 0 {
		t.Fatal("inotify event hasn't been received after 1 second")
	}

	// Try closing the inotify instance
	t.Log("calling Close()")
	watcher.Close()
	t.Log("waiting for the event channel to become closed...")
	select {
	case <-done:
		t.Log("event channel closed")
	case <-time.After(1 * time.Second):
		t.Fatal("event stream was not closed after 1 second")
	}
}

func TestInotifyClose(t *testing.T) {
	watcher, _ := NewWatcher()
	if err := watcher.Close(); err != nil {
		t.Fatalf("close returns: %s", err)
	}
	if watcher.IsValid() {
		t.Fatal("still valid after Close()")
	}

	done := make(chan bool)
	go func() {
		if err := watcher.Close(); err != nil {
			t.Logf("second close returns: %s", err)
		}
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("double Close() test failed: second Close() call didn't return")
	}

	err := watcher.Watch(os.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error on Watch() after Close(), got nil")
	}
}

func TestIgnoredEvents(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}

	dir, err := ioutil.TempDir("", "inotify")
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Add a watch for "_test"
	err = watcher.Watch(dir, nil)
	if err != nil {
		t.Fatalf("Watch failed: %s", err)
	}

	// Receive errors on the error channel on a separate goroutine
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()

	testFileName := dir + "/TestInotifyEvents.testfile"

	// Receive events on the event channel on a separate goroutine
	eventstream := watcher.Event
	var event *Event

	// IN_CREATE, IN_OPEN
	testFile, err := os.OpenFile(testFileName, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("creating test file: %s", err)
	}
	event = <-eventstream
	if event.Mask & IN_CREATE == 0 {
		t.Fatal("inotify hasn't received IN_CREATE")
	}
	event = <-eventstream
	if event.Mask & IN_OPEN == 0 {
		t.Fatal("inotify hasn't received IN_OPEN")
	}

	// IN_CLOSE
	testFile.Close()
	event = <-eventstream
	if event.Mask & IN_CLOSE == 0 {
		t.Fatal("inotify hasn't received IN_CLOSE")
	}

	// IN_DELETE
	if err = os.Remove(testFileName); err != nil {
		t.Fatal("removing test file: %s", err)
	}
	event = <-eventstream
	if event.Mask & IN_DELETE == 0 {
		t.Fatal("inotify hasn't received IN_DELETE")
	}

	// IN_DELETE_SELF, IN_IGNORED
	os.RemoveAll(dir)
	event = <-eventstream
	if event.Mask & (IN_DELETE_SELF | IN_ONLYDIR) == 0 {
		t.Fatal("inotify hasn't received IN_DELETE_SELF")
	}
	event = <-eventstream
	if event.Mask & (IN_IGNORED | IN_ONLYDIR) == 0 {
		t.Fatal("inotify hasn't received IN_IGNORED")
	}

	// mk/rm dir repeatedly
	for j := 0; j < 64; j++ {
		if err = os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll tempdir[%s] again failed: %s", dir, err)
		}
		if err = watcher.Watch(dir, nil); err != nil {
			t.Fatalf("Watch failed: %s", err)
		}
		os.RemoveAll(dir)
		// IN_DELETE_SELF, IN_IGNORED
		event = <-eventstream
		if event.Mask & (IN_DELETE_SELF | IN_ONLYDIR) == 0 {
			t.Fatal("inotify hasn't received IN_DELETE_SELF")
		}
		if event.Name != dir {
			t.Fatalf("received different name event: %s", event)
		}
		event = <-eventstream
		if event.Mask & (IN_IGNORED | IN_ONLYDIR) == 0 {
			t.Fatal("inotify hasn't received IN_IGNORED")
		}
		if event.Name != dir {
			t.Fatalf("received different name event: %s", event)
		}
	}

	if watcher.Len() != 0 {
		t.Fatal("watcher entries should be 0")
	}
	watcher.Close()
}

func TestInotifyOneshot(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}

	dir, err := ioutil.TempDir("", "inotify")
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Add a watch for "_test" with IN_ONESHOT flag
	err = watcher.AddWatch(dir, IN_ALL_EVENTS|IN_ONESHOT, nil)
	if err != nil {
		t.Fatalf("Watch failed: %s", err)
	}

	// Receive errors on the error channel on a separate goroutine
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()

	testFile := dir + "/TestInotifyEvents.testfile"

	// Receive events on the event channel on a separate goroutine
	eventstream := watcher.Event
	var eventsReceived int32 = 0
	done := make(chan bool)
	go func() {
		for event := range eventstream {
			// Only count relevant events
			if event.Name == testFile  || event.Name == dir {
				atomic.AddInt32(&eventsReceived, 1)
				t.Logf("event received: %s", event)
			} else {
				t.Logf("unexpected event received: %s", event)
			}
		}
		done <- true
	}()

	// Create a file
	// This should emits IN_CREATE, IN_OPEN events but only one event must
	// be received with IN_ONESHOT flag
	_, err = os.OpenFile(testFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("creating test file: %s", err)
	}

	// We expect this event to be received almost immediately, but let's wait 1 s to be sure
	time.Sleep(1 * time.Second)
	if atomic.AddInt32(&eventsReceived, 0) != 2 {
		// IN_CREATE and IN_IGNORED
		t.Fatal("inotify event hasn't been received after 1 second")
	}

	// Try closing the inotify instance
	t.Log("calling Close()")
	watcher.Close()
	t.Log("waiting for the event channel to become closed...")
	select {
	case <-done:
		t.Log("event channel closed")
	case <-time.After(1 * time.Second):
		t.Fatal("event stream was not closed after 1 second")
	}
}

func TestFilterEvent(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}
	dir, err := ioutil.TempDir("", "inotify")
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	testFileName := dir + "/TestInotifyEvents.testfile"
	err = watcher.Watch(dir, func(ev *Event) bool {
		return ev.Name == testFileName
	})
	if err != nil {
		t.Fatalf("Watch failed: %s", err)
	}

	// Receive errors on the error channel on a separate goroutine
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()


	// Receive events on the event channel on a separate goroutine
	eventstream := watcher.Event
	var event *Event

	// IN_CREATE, IN_OPEN
	testFile, err := os.OpenFile(testFileName, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("creating test file: %s", err)
	}
	event = <-eventstream
	if event.Mask & IN_CREATE == 0 {
		t.Fatal("inotify hasn't received IN_CREATE")
	}
	event = <-eventstream
	if event.Mask & IN_OPEN == 0 {
		t.Fatal("inotify hasn't received IN_OPEN")
	}

	// IN_CLOSE
	testFile.Close()
	event = <-eventstream
	if event.Mask & IN_CLOSE == 0 {
		t.Fatal("inotify hasn't received IN_CLOSE")
	}

	// IN_DELETE
	if err = os.Remove(testFileName); err != nil {
		t.Fatal("removing test file: %s", err)
	}
	event = <-eventstream
	if event.Mask & IN_DELETE == 0 {
		t.Fatal("inotify hasn't received IN_DELETE")
	}

	// unrelated filename
	dummyFileName := dir + "/TestInotifyEvents.dummyfile"
	dummyFile, err := os.OpenFile(dummyFileName, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("creating test file: %s", err)
	}
	defer dummyFile.Close()
	event = nil
	select {
	case event = <-eventstream:
	default:
	}
	if event != nil {
		t.Fatal("receive unrelated event: %v", event)
	}

	watcher.Close()
}

func TestRemoveWatch(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}
	dir, err := ioutil.TempDir("", "inotify")
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	if err = watcher.Watch(dir, nil); err != nil {
		t.Fatalf("Watch failed: %s", err)
	}

	if err = watcher.RemoveWatch(dir); err != nil {
		t.Fatalf("RemoveWatch failed: %s, err")
	}

	var event *Event
	select {
	case event = <-watcher.Event:
	default:
	}
	if event == nil {
		t.Fatal("failed to receive event")
	}
	if event.Mask & IN_IGNORED == 0 {
		t.Fatal("no IN_IGNORE flag in the event")
	}
}
	
