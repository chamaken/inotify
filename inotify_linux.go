// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Package inotify implements a wrapper for the Linux inotify system.

Example:
    watcher, err := inotify.NewWatcher()
    if err != nil {
        log.Fatal(err)
    }
    err = watcher.Watch("/tmp")
    if err != nil {
        log.Fatal(err)
    }
    for {
        select {
        case ev := <-watcher.Event:
            log.Println("event:", ev)
        case err := <-watcher.Error:
            log.Println("error:", err)
        }
    }

Note from inotify(7):

    IN_IGNORED        Watch  was removed explicitly (inotify_rm_watch(2)) or
                      automatically (file was deleted, or file system was unmounted).

You will receive the event in case of not only RemoveWatch() but also remove a
watch file / directory. In addition to it, you will receive IN_IGNORED if
IN_ONESHOT flag to AddWatch().
*/
package inotify

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const EPOLL_MAX_EVENTS = 64

type Event struct {
	Mask   uint32 // Mask of events
	Cookie uint32 // Unique cookie associating related events (for rename(2))
	Name   string // File name (optional)
}

type watch struct {
	wd     uint32            // Watch descriptor (as returned by the inotify_add_watch() syscall)
	flags  uint32            // inotify flags of this watch (see inotify(7) for the list of valid flags)
	filter func(*Event) bool // filter function which retuns true if got interested event
}

type Watcher struct {
	mu      sync.Mutex
	cond    *sync.Cond        // sync removing on rm_watch with IN_IGNORE
	fd      int               // File descriptor (as returned by the inotify_init() syscall)
	watches map[string]*watch // Map of inotify watches (key: path)
	paths   map[int]string    // Map of watched paths (key: watch descriptor)
	Error   chan error        // Errors are sent on this channel
	Event   chan *Event       // Events are returned on this channel
	done    chan bool         // Channel for sending a "quit message" to the reader goroutine
	closed  chan bool         // Channel for syncing Close()s
	running bool              // is go routine running?
}

// NewWatcher creates and returns a new inotify instance using inotify_init(2)
func NewWatcher() (*Watcher, error) {
	fd, errno := syscall.InotifyInit()
	if fd == -1 {
		return nil, os.NewSyscallError("inotify_init", errno)
	}
	w := &Watcher{
		fd:      fd,
		watches: make(map[string]*watch),
		paths:   make(map[int]string),
		Event:   make(chan *Event),
		Error:   make(chan error),
		done:    make(chan bool),
		closed:  make(chan bool),
	}
	w.cond = sync.NewCond(&w.mu)

	rp, wp, err := os.Pipe() // for done
	if err != nil {
		return nil, err
	}
	go func() {
		<-w.done
		wp.Close()
	}()

	epfd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, os.NewSyscallError("epoll_create1", err)
	}
	event := &syscall.EpollEvent{syscall.EPOLLIN, int32(w.fd), 0}
	if err = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, w.fd, event); err != nil {
		return nil, os.NewSyscallError("epoll_ctl", err)
	}
	event = &syscall.EpollEvent{syscall.EPOLLIN, int32(rp.Fd()), 0}
	if err = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, int(rp.Fd()), event); err != nil {
		return nil, os.NewSyscallError("epoll_ctl", err)
	}

	go w.epollEvents(epfd, rp)

	return w, nil
}

// Close closes an inotify watcher instance
// It sends a message to the reader goroutine to quit and removes all watches
// associated with the inotify instance
func (w *Watcher) Close() error {
	if w.fd < 0 {
		return os.NewSyscallError("closed", syscall.EBADF)
	}

	// Send "quit" message to the reader goroutine
	// then channels are closed in epollEvents()
	w.done <- true
	// And wait receiving it's actually closed
	<-w.closed

	if err := syscall.Close(w.fd); err != nil {
		// XXX: become inconsistent state
		return os.NewSyscallError("close", err)
	}
	w.fd = -1 // invalidate inotify descriptor

	// inotify(7):
	// When  all file descriptors referring to an inotify instance have been
	// closed, ...; all associated watches are automatically freed.
	w.watches = nil
	w.paths = nil
	w.cond = nil

	return nil
}

// AddWatch adds path to the watched file set.
// The flags are interpreted as described in inotify_add_watch(2).
func (w *Watcher) AddWatch(path string, flags uint32) error {
	return w.AddWatchFilter(path, flags, nil)
}

func (w *Watcher) AddWatchFilter(path string, flags uint32, filter func(*Event) bool) error {
	if w.fd < 0 {
		return os.NewSyscallError("closed", syscall.EBADF)
	}

	w.mu.Lock() // synchronization of Watcher map
	defer w.mu.Unlock()
	watchEntry, found := w.watches[path]
	if found {
		watchEntry.flags |= flags
		flags |= syscall.IN_MASK_ADD
	}

	wd, err := syscall.InotifyAddWatch(w.fd, path, flags)
	if err != nil {
		return &os.PathError{
			Op:   "inotify_add_watch",
			Path: path,
			Err:  err,
		}
	}

	if found {
		// wd seems not changing, just updating
		// but I could not found its express term
		delete(w.paths, int(watchEntry.wd))
	}
	w.watches[path] = &watch{wd: uint32(wd), flags: flags, filter: filter}
	w.paths[wd] = path

	return nil
}

// Watch adds path to the watched file set, watching all events.
func (w *Watcher) Watch(path string) error {
	return w.AddWatchFilter(path, IN_ALL_EVENTS, nil)
}
func (w *Watcher) WatchFilter(path string, filter func(*Event) bool) error {
	return w.AddWatchFilter(path, IN_ALL_EVENTS, filter)
}

// RemoveWatch removes path from the watched file set.
func (w *Watcher) RemoveWatch(path string) error {
	if w.fd < 0 {
		return os.NewSyscallError("closed", syscall.EBADF)
	}

	w.mu.Lock() // synchronization of Watcher map
	defer w.mu.Unlock()
	watch, ok := w.watches[path]
	if !ok {
		return errors.New(fmt.Sprintf("can't remove non-existent inotify watch for: %s", path))
	}
	success, errno := syscall.InotifyRmWatch(w.fd, watch.wd)
	if success == -1 {
		return os.NewSyscallError("inotify_rm_watch", errno)
	}
	for ok {
		w.cond.Wait()
		_, ok = w.watches[path]
	}
	return nil
}

func (w *Watcher) epollEvents(epfd int, donef *os.File) {
	w.running = true
	defer func() {
		syscall.Close(epfd)
		w.running = false
		w.closed <- true
	}()
	events := make([]syscall.EpollEvent, EPOLL_MAX_EVENTS)
	donefd := int32(donef.Fd())
	for {
		nevents, err := syscall.EpollWait(epfd, events, -1)
		if err != nil {
			w.Error <- os.NewSyscallError("epoll_wait", err)
			continue
		}
		if nevents == 0 {
			continue
		}

		for i := 0; i < nevents; i++ {
			if events[i].Fd == donefd {
				if err = donef.Close(); err != nil {
					w.Error <- err
				}
				close(w.done)
				close(w.Event)
				close(w.Error)
				return
			} else if events[i].Fd != int32(w.fd) {
				continue
			}
			if err = w.readEvents(); err != nil {
				w.Error <- err
			}
		}
	}
}

// readEvents reads from the inotify file descriptor, converts the
// received events into Event objects and sends them via the Event channel
func (w *Watcher) readEvents() error {
	var buf [syscall.SizeofInotifyEvent * 4096]byte

	n, err := syscall.Read(w.fd, buf[:])

	switch {
	case err == syscall.EINTR:
		return nil
	case err != nil:
		return os.NewSyscallError("read", err)
	case n == 0:
		return errors.New("inotify: possibilly too many events occurred at once")
	case n < syscall.SizeofInotifyEvent:
		// brefore: syscall.Read
		//   while n < syscall.SizeofInotifyEvent:
		// 	  syscall.Ioctl(w.fd, syscall.FIONREAD, &n);
		//
		return errors.New("inotify: short read in readEvents()")
	}

	var offset uint32 = 0
	// We don't know how many events we just read into the buffer
	// While the offset points to at least one whole event...
	for offset <= uint32(n-syscall.SizeofInotifyEvent) {
		// Point "raw" to the event in the buffer
		raw := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		event := new(Event)
		event.Mask = uint32(raw.Mask)
		event.Cookie = uint32(raw.Cookie)
		nameLen := uint32(raw.Len)
		// If the event happened to the watched directory or the watched file, the kernel
		// doesn't append the filename to the event, but we would like to always fill the
		// the "Name" field with a valid filename. We retrieve the path of the watch from
		// the "paths" map.
		w.mu.Lock()
		event.Name = w.paths[int(raw.Wd)]
		filter := w.watches[event.Name].filter
		if event.Mask&IN_IGNORED != 0 {
			delete(w.paths, int(raw.Wd))
			delete(w.watches, event.Name)
		}
		w.cond.Broadcast()
		w.mu.Unlock()
		if nameLen > 0 {
			// Point "bytes" at the first byte of the filename
			bytes := (*[syscall.PathMax]byte)(unsafe.Pointer(&buf[offset+syscall.SizeofInotifyEvent]))
			// The filename is padded with NUL bytes. TrimRight() gets rid of those.
			event.Name += "/" + strings.TrimRight(string(bytes[0:nameLen]), "\000")
		}
		// Send the event on the events channel
		if filter == nil || filter(event) {
			w.Event <- event
		}

		// Move to the next event in the buffer
		offset += syscall.SizeofInotifyEvent + nameLen
	}

	return nil
}

func (w *Watcher) length() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.watches) != len(w.paths) {
		panic("internal maps lengh is differ")
	}
	return len(w.watches)
}

// String formats the event e in the form
// "filename: 0xEventMask = IN_ACCESS|IN_ATTRIB_|..."
func (e *Event) String() string {
	var events string = ""

	m := e.Mask
	for _, b := range eventBits {
		if m&b.Value != 0 {
			m &^= b.Value
			events += "|" + b.Name
		}
	}

	if m != 0 {
		events += fmt.Sprintf("|%#x", m)
	}
	if len(events) > 0 {
		events = " == " + events[1:]
	}

	return fmt.Sprintf("%q: %#x%s", e.Name, e.Mask, events)
}

const (
	// Options for inotify_init() are not exported
	// IN_CLOEXEC    uint32 = syscall.IN_CLOEXEC
	// IN_NONBLOCK   uint32 = syscall.IN_NONBLOCK

	// Options for AddWatch
	IN_DONT_FOLLOW uint32 = syscall.IN_DONT_FOLLOW
	IN_ONESHOT     uint32 = syscall.IN_ONESHOT
	IN_ONLYDIR     uint32 = syscall.IN_ONLYDIR

	// The "IN_MASK_ADD" option is not exported, as AddWatch
	// adds it automatically, if there is already a watch for the given path
	// IN_MASK_ADD      uint32 = syscall.IN_MASK_ADD

	// Events
	IN_ACCESS        uint32 = syscall.IN_ACCESS
	IN_ALL_EVENTS    uint32 = syscall.IN_ALL_EVENTS
	IN_ATTRIB        uint32 = syscall.IN_ATTRIB
	IN_CLOSE         uint32 = syscall.IN_CLOSE
	IN_CLOSE_NOWRITE uint32 = syscall.IN_CLOSE_NOWRITE
	IN_CLOSE_WRITE   uint32 = syscall.IN_CLOSE_WRITE
	IN_CREATE        uint32 = syscall.IN_CREATE
	IN_DELETE        uint32 = syscall.IN_DELETE
	IN_DELETE_SELF   uint32 = syscall.IN_DELETE_SELF
	IN_MODIFY        uint32 = syscall.IN_MODIFY
	IN_MOVE          uint32 = syscall.IN_MOVE
	IN_MOVED_FROM    uint32 = syscall.IN_MOVED_FROM
	IN_MOVED_TO      uint32 = syscall.IN_MOVED_TO
	IN_MOVE_SELF     uint32 = syscall.IN_MOVE_SELF
	IN_OPEN          uint32 = syscall.IN_OPEN

	// Special events
	IN_ISDIR      uint32 = syscall.IN_ISDIR
	IN_IGNORED    uint32 = syscall.IN_IGNORED
	IN_Q_OVERFLOW uint32 = syscall.IN_Q_OVERFLOW
	IN_UNMOUNT    uint32 = syscall.IN_UNMOUNT
)

var eventBits = []struct {
	Value uint32
	Name  string
}{
	{IN_ACCESS, "IN_ACCESS"},
	{IN_ATTRIB, "IN_ATTRIB"},
	{IN_CLOSE, "IN_CLOSE"},
	{IN_CLOSE_NOWRITE, "IN_CLOSE_NOWRITE"},
	{IN_CLOSE_WRITE, "IN_CLOSE_WRITE"},
	{IN_CREATE, "IN_CREATE"},
	{IN_DELETE, "IN_DELETE"},
	{IN_DELETE_SELF, "IN_DELETE_SELF"},
	{IN_MODIFY, "IN_MODIFY"},
	{IN_MOVE, "IN_MOVE"},
	{IN_MOVED_FROM, "IN_MOVED_FROM"},
	{IN_MOVED_TO, "IN_MOVED_TO"},
	{IN_MOVE_SELF, "IN_MOVE_SELF"},
	{IN_OPEN, "IN_OPEN"},
	{IN_ISDIR, "IN_ISDIR"},
	{IN_IGNORED, "IN_IGNORED"},
	{IN_Q_OVERFLOW, "IN_Q_OVERFLOW"},
	{IN_UNMOUNT, "IN_UNMOUNT"},
}
