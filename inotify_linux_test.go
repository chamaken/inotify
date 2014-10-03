// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package inotify

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var TIMEOUT = 100 * time.Millisecond

const TMP_PREFIX = "_inotify_tmp"

func init() {
	dir, _ := ioutil.TempDir("", TMP_PREFIX)
	parent := filepath.Dir(dir)
	globs, _ := filepath.Glob(filepath.Join(parent, TMP_PREFIX+"*"))
	for _, d := range globs {
		os.RemoveAll(d)
	}
}

func TestInotifyEvents(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}

	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Add a watch for "_test"
	err = watcher.Watch(dir)
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

	// We expect this event to be received almost immediately, but let's wait 10ms to be sure
	time.Sleep(TIMEOUT)
	if atomic.AddInt32(&eventsReceived, 0) == 0 {
		t.Fatal("inotify event hasn't been received after 100ms")
	}

	// Try closing the inotify instance
	watcher.Close()
	t.Log("waiting for the event channel to become closed...")
	select {
	case <-done:
		t.Log("event channel closed")
	case <-time.After(TIMEOUT):
		t.Fatal("event stream was not closed after 100ms")
	}
}

func TestInotifyClose(t *testing.T) {
	watcher, _ := NewWatcher()
	if err := watcher.Close(); err != nil {
		t.Fatalf("close returns: %s", err)
	}
	if watcher.running {
		t.Fatal("still valid after Close()")
	}

	done := make(chan bool)
	go func() {
		if err := watcher.Close(); err == nil {
			t.Fatalf("Close() should return err")
		}
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(TIMEOUT):
		t.Fatal("double Close() test failed: second Close() call didn't return")
	}

	err := watcher.Watch(os.TempDir())
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

	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Add a watch for "_test"
	err = watcher.Watch(dir)
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
	if event.Mask&IN_CREATE == 0 {
		t.Fatal("inotify hasn't received IN_CREATE")
	}
	event = <-eventstream
	if event.Mask&IN_OPEN == 0 {
		t.Fatal("inotify hasn't received IN_OPEN")
	}

	// IN_CLOSE
	testFile.Close()
	event = <-eventstream
	if event.Mask&IN_CLOSE == 0 {
		t.Fatal("inotify hasn't received IN_CLOSE")
	}

	// IN_DELETE
	if err = os.Remove(testFileName); err != nil {
		t.Fatal("removing test file: %s", err)
	}
	event = <-eventstream
	if event.Mask&IN_DELETE == 0 {
		t.Fatal("inotify hasn't received IN_DELETE")
	}

	// IN_DELETE_SELF, IN_IGNORED
	os.RemoveAll(dir)
	event = <-eventstream
	if event.Mask&(IN_DELETE_SELF|IN_ONLYDIR) == 0 {
		t.Fatal("inotify hasn't received IN_DELETE_SELF")
	}
	event = <-eventstream
	if event.Mask&(IN_IGNORED|IN_ONLYDIR) == 0 {
		t.Fatal("inotify hasn't received IN_IGNORED")
	}

	// mk/rm dir repeatedly
	for j := 0; j < 64; j++ {
		if err = os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll tempdir[%s] again failed: %s", dir, err)
		}
		if err = watcher.Watch(dir); err != nil {
			t.Fatalf("Watch failed: %s", err)
		}
		os.RemoveAll(dir)
		// IN_DELETE_SELF, IN_IGNORED
		event = <-eventstream
		if event.Mask&(IN_DELETE_SELF|IN_ONLYDIR) == 0 {
			t.Fatal("inotify hasn't received IN_DELETE_SELF")
		}
		if event.Name != dir {
			t.Fatalf("received different name event: %s", event.Name)
		}
		event = <-eventstream
		if event.Mask&(IN_IGNORED|IN_ONLYDIR) == 0 {
			t.Fatal("inotify hasn't received IN_IGNORED")
		}
		if event.Name != dir {
			t.Fatalf("received different name event: %s", event.Name)
		}
	}
	// repeat again, changing dirname and append / at last
	for j := 0; j < 64; j++ {
		name := fmt.Sprintf("%s/%d/", dir, j)
		if err = os.MkdirAll(name, 0755); err != nil {
			t.Fatalf("MkdirAll tempdir[%s] again failed: %s", dir, err)
		}
		if err = watcher.Watch(name); err != nil {
			t.Fatalf("Watch failed: %s", err)
		}
		os.RemoveAll(name)
		// IN_DELETE_SELF, IN_IGNORED
		event = <-eventstream
		if event.Mask&(IN_DELETE_SELF|IN_ONLYDIR) == 0 {
			t.Fatal("inotify hasn't received IN_DELETE_SELF")
		}
		if event.Name != name {
			t.Fatalf("received different name event: %s", event.Name)
		}
		event = <-eventstream
		if event.Mask&(IN_IGNORED|IN_ONLYDIR) == 0 {
			t.Fatal("inotify hasn't received IN_IGNORED")
		}
		if event.Name != name {
			t.Fatalf("received different name event: %s", event.Name)
		}
	}

	// wait for catching up to inotify IGNORE event
	time.Sleep(TIMEOUT)
	if watcher.length() != 0 {
		t.Fatalf("watcher entries should be 0, but got: %d", watcher.length())
	}
	watcher.Close()
	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}

func TestInotifyOneshot(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}

	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Add a watch for "_test" with IN_ONESHOT flag
	err = watcher.AddWatchFilter(dir, IN_ALL_EVENTS|IN_ONESHOT, nil)
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
			if event.Name == testFile || event.Name == dir {
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

	// We expect this event to be received almost immediately, but let's wait 10ms to be sure
	time.Sleep(TIMEOUT)
	if atomic.AddInt32(&eventsReceived, 0) != 2 {
		// IN_CREATE and IN_IGNORED
		t.Fatal("inotify event hasn't been received after 100ms")
	}

	// Try closing the inotify instance
	watcher.Close()
	t.Log("waiting for the event channel to become closed...")
	select {
	case <-done:
		t.Log("event channel closed")
	case <-time.After(TIMEOUT):
		t.Fatal("event stream was not closed after 100ms")
	}
}

func TestFilterEvent(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}
	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	testFileName := dir + "/TestInotifyEvents.testfile"
	err = watcher.WatchFilter(dir, func(ev *Event) bool {
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
	t.Logf("event received: %s", event)
	if event.Mask&IN_CREATE == 0 {
		t.Fatal("inotify hasn't received IN_CREATE")
	}
	event = <-eventstream
	t.Logf("event received: %s", event)
	if event.Mask&IN_OPEN == 0 {
		t.Fatal("inotify hasn't received IN_OPEN")
	}

	// IN_CLOSE
	testFile.Close()
	event = <-eventstream
	t.Logf("event received: %s", event)
	if event.Mask&IN_CLOSE == 0 {
		t.Fatal("inotify hasn't received IN_CLOSE")
	}

	// IN_DELETE
	if err = os.Remove(testFileName); err != nil {
		t.Fatal("removing test file: %s", err)
	}
	event = <-eventstream
	t.Logf("event received: %s", event)
	if event.Mask&IN_DELETE == 0 {
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
		t.Fatal("receive unrelated event: %v", event)
	case <-time.After(TIMEOUT):
	}

	watcher.Close()
	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}

func TestRemoveWatch(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}
	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	if err = watcher.Watch(dir); err != nil {
		t.Fatalf("Watch failed: %s", err)
	}

	if err = watcher.RemoveWatch(dir); err != nil {
		t.Fatalf("RemoveWatch failed: %s, err")
	}

	var event *Event
	select {
	case event = <-watcher.Event:
		if event.Mask&IN_IGNORED == 0 {
			t.Fatal("no IN_IGNORE flag in the event")
		}
	case <-time.After(TIMEOUT):
		t.Fatal("failed to receive event")

	}
	watcher.Close()
	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}

func TestSamename(t *testing.T) {
	// Create an inotify watcher instance and initialize it
	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}

	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// Receive errors on the error channel on a separate goroutine
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()

	// Receive events on the event channel on a separate goroutine
	eventstream := watcher.Event
	var event *Event
	testFileName := dir + "/TestInotifyEvents.testfile"

	// IN_OPEN
	testFile, err := os.OpenFile(testFileName, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("creating test file: %s", err)
	}

	err = watcher.AddWatch(testFileName, IN_CLOSE)
	// err = watcher.AddWatch(testFileName, IN_CLOSE | IN_MODIFY)
	if err != nil {
		t.Fatalf("AddWatch failed: %s", err)
	}

	if _, err = testFile.Write(([]byte)("test line 1")); err != nil {
		t.Fatalf("writing to test file: %s", err)
	}
	if err = testFile.Sync(); err != nil {
		t.Fatal("sync test file: %s", err)
	}

	select {
	case event = <-eventstream:
		t.Fatalf("should not receive event, but got: %s, mask: %08x",
			event.Name, event.Mask)
	case <-time.After(TIMEOUT):
	}

	err = watcher.AddWatch(testFileName, IN_MODIFY)
	if err != nil {
		t.Fatalf("AddWatch failed: %s", err)
	}
	if _, err = testFile.Write(([]byte)("test line 2")); err != nil {
		t.Fatalf("writing to test file: %s", err)
	}
	if err = testFile.Sync(); err != nil {
		t.Fatalf("sync test file: %s", err)
	}

	select {
	case event = <-eventstream:
		if event.Mask&IN_MODIFY == 0 {
			t.Fatalf("should receive IN_MODIFY, but got: %08x",
				event.Mask)
		}
		if event.Name != testFile.Name() {
			t.Fatalf("should receive name: %s, but got: %s",
				testFile.Name(), event.Name)
		}
	case <-time.After(TIMEOUT):
		t.Fatal("should receive IN_MODIFY event, but got nothing")
	}

	// IN_CLOSE
	testFile.Close()
	select {
	case event = <-eventstream:
		if event.Mask&IN_CLOSE == 0 {
			t.Fatalf("should receive IN_CLOSE, but got: %08x",
				event.Mask)
		}
		if event.Name != testFile.Name() {
			t.Fatalf("should receive name: %s, but got: %s",
				testFile.Name(), event.Name)
		}
	case <-time.After(TIMEOUT):
		t.Fatal("should receive IN_CLOSE event, but got nothing")
	}

	// IN_DELETE & IN_IGNORED, but should get IN_IGNORED only
	if err = os.Remove(testFileName); err != nil {
		t.Fatalf("removing test file: %s", err)
	}
	select {
	case event = <-eventstream:
		if event.Mask&IN_IGNORED == 0 {
			t.Fatalf("should receive IN_IGNORED, but got: %08x",
				event.Mask)
		}
		if event.Name != testFile.Name() {
			t.Fatalf("should receive name: %s, but got: %s",
				testFile.Name(), event.Name)
		}
	case <-time.After(TIMEOUT):
	}
	select {
	case event = <-eventstream:
		t.Fatalf("should not receive event, but got: %s, mask: %08x",
			event.Name, event.Mask)
	case <-time.After(TIMEOUT):
	}

	if err = watcher.Close(); err != nil {
		t.Fatalf("error on close: %s", err)
	}

	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}

func TestRemoveClose(t *testing.T) {
	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()
	done := make(chan bool)
	go func() {
		var i, j int
		for e := range watcher.Event {
			testFileName := filepath.Join(dir, fmt.Sprintf("TestInotifyEvents.%d", i))
			if e.Name != testFileName {
				t.Fatalf("should be %s, but got: %s", testFileName, e.Name)
			}
			// t.Logf("event: %s", e)
			switch j % 3 {
			case 0:
				if e.Mask&IN_ATTRIB == 0 { // I do not know why
					t.Fatalf("should be IN_ATTRIB, but: 0x%x", e.Mask)
				}
			case 1:
				if e.Mask&IN_DELETE_SELF == 0 {
					t.Fatalf("should be IN_DELETE_SELF, but: 0x%x", e.Mask)
				}
			case 2:
				if e.Mask&IN_IGNORED == 0 {
					t.Fatalf("should be IN_IGNORE, but: 0x%x", e.Mask)
				}
			}
			j++
			if j%3 == 0 {
				i++
			}
			if j == 3000 {
				break
			}
		}
		done <- true
	}()

	for i := 0; i < 1000; i++ {
		testFileName := filepath.Join(dir, fmt.Sprintf("TestInotifyEvents.%d", i))
		testFile, err := os.OpenFile(testFileName, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			t.Fatalf("creating test file: %s", err)
		}
		if err = testFile.Close(); err != nil {
			t.Fatalf("close test file: %s", err)
		}
		if err = watcher.Watch(testFileName); err != nil {
			t.Fatalf("Watch(%s) failed: %s", testFileName, err)
		}
		if err = os.Remove(testFileName); err != nil {
			t.Fatalf("remove test file: %s", err)
		}
	}

	// wait for catching up all event
	<-done
	if watcher.length() != 0 {
		t.Fatalf("watcher entries should be 0, but got: %d", watcher.length())
	}

	go func() {
		watcher.Close()
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(TIMEOUT):
		t.Fatal("Close() not returned")
	}

	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}

func TestRmdirClose(t *testing.T) {
	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher failed: %s", err)
	}
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()
	done := make(chan bool)
	go func() {
		var i, j int
		for e := range watcher.Event {
			testName := filepath.Join(dir, fmt.Sprintf("TestInotifyEvents.%d", i))
			if e.Name != testName {
				t.Fatalf("should be %s, but got: %s", testName, e.Name)
			}
			// t.Logf("event: %s", e)
			switch j % 2 {
			case 0:
				if e.Mask&IN_DELETE_SELF == 0 {
					t.Fatalf("should be IN_DELETE_SELF, but: 0x%x", e.Mask)
				}
			case 1:
				if e.Mask&IN_IGNORED == 0 {
					t.Fatalf("should be IN_IGNORE, but: 0x%x", e.Mask)
				}
			}
			j++
			if j%2 == 0 {
				i++
			}
			if j == 2000 {
				break
			}
		}
		done <- true
	}()

	for i := 0; i < 1000; i++ {
		testDir := filepath.Join(dir, fmt.Sprintf("TestInotifyEvents.%d", i))
		err := os.Mkdir(testDir, 0700)
		if err != nil {
			t.Fatalf("creating test dir: %s", err)
		}
		if err = watcher.Watch(testDir); err != nil {
			t.Fatalf("Watch(%s) failed: %s", testDir, err)
		}
		if err = os.Remove(testDir); err != nil {
			t.Fatalf("remove test dir: %s", err)
		}
	}

	// wait for all event
	<-done
	if watcher.length() != 0 {
		t.Fatalf("watcher entries should be 0, but got: %d", watcher.length())
	}

	go func() {
		watcher.Close()
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(TIMEOUT):
		t.Fatal("Close() not returned")
	}

	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}

func TestRemoveAll(t *testing.T) {
	dir, err := ioutil.TempDir("", TMP_PREFIX)
	if err != nil {
		t.Fatalf("TempDir failed: %s", err)
	}
	defer os.RemoveAll(dir)

	// create 10 removing parent dirs in the tmpdir
	// and 10 files in a removing dir
	nevents := make(map[string]int)
	var testDirs [100]string
	for i := 0; i < 100; i++ {
		testDirs[i] = filepath.Join(dir, fmt.Sprintf("rmtest%d", i))
		err = os.Mkdir(testDirs[i], 0700)
		if err != nil {
			t.Fatalf("failed to create testDir: %s", err)
		}
		nevents[testDirs[i]] = 2 // events &IN_ISDIR == 0
		for j := 0; j < 10; j++ {
			fname := filepath.Join(testDirs[i], fmt.Sprintf("inotify_testfile%d", j))
			testFile, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, 0666)
			if err != nil {
				t.Fatalf("failed to create testfile: %s", err)
			}
			if err := testFile.Close(); err != nil {
				t.Fatalf("failed to close testfile: %s", err)
			}
			nevents[fname] = 1 // IN_DELETE
		}
	}

	watcher, err := NewWatcher()
	if err != nil {
		t.Fatalf("could not create TailWatcher: %s", err)
	}
	go func() {
		for err := range watcher.Error {
			t.Fatalf("error received: %s", err)
		}
	}()
	done := make(chan bool)
	valid_mask := (IN_OPEN | IN_ISDIR | IN_ACCESS | IN_DELETE | IN_CLOSE | IN_DELETE_SELF | IN_IGNORED)
	go func() {
		var i int
		for e := range watcher.Event {
			// go's RemoveAll(dir), a dir having 10 files receives 16 events
			//
			//   IN_OPEN|IN_ISDIR		dir
			//   IN_ACCESS|IN_ISDIR		dir multuple times?
			//   IN_DELETE 			x 10 files
			//   IN_ACCESS|IN_ISDIR		dir
			//   IN_CLOSE|IN_ISDIR		dir multiple? | IN_CLOSE_WRITE
			//   IN_DELETE_SELF		dir
			//   IN_IGNORED			dir
			//
			// the order seems to be random
			// t.Logf("receive event[%d]: %s", i, e)
			if !strings.HasPrefix(e.Name, dir) { // testDirs[j])
				t.Fatalf("should have %s, but got: %s", dir, e.Name)
			}
			if e.Mask&^valid_mask != 0 {
				t.Fatalf("receive invalid mask: %s", e)
			}
			if e.Mask&IN_ISDIR == 0 {
				nevents[e.Name] = nevents[e.Name] - 1
				i++
			}
			if nevents[e.Name] < 0 {
				t.Fatalf("receive unexpected event: %s", e)
			}
			/*
				switch i % 16 {
				case 0:
					if e.Mask&(IN_OPEN|IN_ISDIR) != (IN_OPEN | IN_ISDIR) {
						t.Fatalf("should be IN_OPEN|IN_ISDIR, but got 0x%x", e.Mask)
					}
				case 1:
					if e.Mask&(IN_ACCESS|IN_ISDIR) != (IN_ACCESS | IN_ISDIR) {
						t.Fatalf("should be IN_ACCESS|IN_ISDIR, but got 0x%x", e.Mask)
					}
				case 12:
					if e.Mask&(IN_ACCESS|IN_ISDIR) != (IN_ACCESS | IN_ISDIR) {
						t.Fatalf("should be IN_ACCESS|IN_ISDIR, but got 0x%x", e.Mask)
					}
				case 13:
					if e.Mask&(IN_CLOSE|IN_ISDIR) == (IN_CLOSE | IN_ISDIR) {
						t.Fatalf("should be IN_CLOSE|IN_ISDIR, but got 0x%x", e.Mask)
					}
				case 14:
					if e.Mask&IN_DELETE_SELF != IN_DELETE_SELF {
						t.Fatalf("should be IN_DELETE_SELF, but got: 0x%x", e.Mask)
					}
				case 15:
					if e.Mask&IN_IGNORED != IN_IGNORED {
						t.Fatalf("should be IN_IGNORE, but got: 0x%x", e.Mask)
					}
				default:
					fprefix := filepath.Join(testDirs[j], "inotify_testfile")
					if !strings.HasPrefix(e.Name, fprefix) {
						t.Fatalf("should have %s, but got: %s", fprefix, e.Name)
					}
					if e.Mask&IN_DELETE != IN_DELETE {
						t.Fatalf("should be IN_DELETE, but got: 0x%x", e.Mask)
					}
				}
			*/
			if i == 1200 {
				// 100 dirs ( x 2 events) , each have 10 files (one event)
				break
			}
		}
		done <- true
	}()

	for i := 0; i < 100; i++ {
		if err = watcher.Watch(testDirs[i]); err != nil {
			t.Fatalf("failed to Add to Watcher: %s", err)
		}
	}
	for i := 0; i < 100; i++ {
		if err = os.RemoveAll(testDirs[i]); err != nil {
			t.Fatalf("failed to RemoveAll: %s", err)
		}
	}

	<-done
	if watcher.length() != 0 {
		for k, _ := range watcher.watches {
			t.Logf("still watching path: %s", k)
		}
		t.Fatalf("watcher entries should be 0, but got: %d", watcher.length())
	}

	watcher.Close()
	if watcher.running {
		t.Fatal("still valid after Close()")
	}
}
