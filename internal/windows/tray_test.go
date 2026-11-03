//go:build windows && (amd64 || arm64)

package windows

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"slices"
	"syscall"
	"testing"

	"github.com/egoist/mygo/internal/platform"
)

// TestTrayNumber numbers icons by their IDs, whatever order they come in,
// apart from the numbers NewTray counts.
func TestTrayNumber(t *testing.T) {
	number := func(ids ...string) map[string]uint32 {
		b := &Backend{trays: map[uint32]*tray{}}
		out := map[string]uint32{}
		for _, id := range ids {
			b.nextTray++
			tr := &tray{b: b, id: b.nextTray}
			b.trays[tr.id] = tr
			tr.SetID(id)
			if b.trays[tr.id] != tr || len(b.trays) != int(b.nextTray) {
				t.Fatalf("%q: the icons are %v", id, b.trays)
			}
			out[id] = tr.id
		}
		return out
	}
	a, b := number("app.cpu", "app.memory"), number("app.memory", "app.cpu")
	for id, n := range a {
		if b[id] != n {
			t.Errorf("%q is %#x, then %#x", id, n, b[id])
		}
		if n&(1<<31) == 0 {
			t.Errorf("%q is %#x, which NewTray may count to", id, n)
		}
	}
	if a["app.cpu"] == a["app.memory"] {
		t.Error("two IDs have one number")
	}
	// Without an ID, or with one another icon has, an icon keeps its own.
	if n := number("")[""]; n != 1 {
		t.Errorf("an icon without an ID is %#x", n)
	}
	if twice := number("app.cpu", "app.cpu"); twice["app.cpu"] != 2 {
		t.Errorf("an icon with the ID of another is %#x", twice["app.cpu"])
	}
}

// TestWhiten draws a template image white on a dark taskbar, as macOS
// tints it, keeping its shape.
func TestWhiten(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{0, 0, 0, 255})
	src.SetNRGBA(1, 0, color.NRGBA{0, 0, 0, 56})
	var b bytes.Buffer
	if err := png.Encode(&b, src); err != nil {
		t.Fatal(err)
	}
	out, err := whiten(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	for x, a := range []uint8{255, 56} {
		if c := color.NRGBAModel.Convert(img.At(x, 0)).(color.NRGBA); c != (color.NRGBA{255, 255, 255, a}) {
			t.Errorf("pixel %d is %v", x, c)
		}
	}
}

// TestTraysAppearanceTemporary turns the taskbar dark or light while a
// notification's temporary icon shows: it has no handler to tell.
func TestTraysAppearanceTemporary(t *testing.T) {
	b := &Backend{trays: map[uint32]*tray{}}
	tr := &tray{b: b, id: 1, temporary: true}
	tr.dark = !tr.Dark()
	b.trays[tr.id] = tr
	b.traysAppearance()
}

// shellCall is what a test sees of a call to the notification area.
type shellCall struct {
	nim  uintptr
	data notifyIconData
}

// recordShell keeps a test out of the user's notification area, recording
// what the app tells the shell.
func recordShell(t *testing.T) *[]shellCall {
	t.Helper()
	var calls []shellCall
	old := shellNotifyIcon
	shellNotifyIcon = func(nim uintptr, d *notifyIconData) bool {
		calls = append(calls, shellCall{nim, *d})
		return true
	}
	t.Cleanup(func() { shellNotifyIcon = old })
	return &calls
}

// balloonTitles are the titles of the balloons a test showed.
func balloonTitles(calls []shellCall) []string {
	var titles []string
	for _, c := range calls {
		if c.nim == nimModify && c.data.InfoFlags&niifUser != 0 {
			titles = append(titles, syscall.UTF16ToString(c.data.InfoTitle[:]))
		}
	}
	return titles
}

// notificationBackend has one tray icon, as an app with a tray does.
func notificationBackend() (*Backend, *tray) {
	b := New()
	tr := &tray{b: b, id: 1, added: true}
	b.trays[tr.id] = tr
	b.nextTray = 1
	return b, tr
}

// TestNotificationsShareOneTray shows several notifications one after
// another on the app's tray icon: none takes an icon of its own, and the
// last one takes the balloon's place.
func TestNotificationsShareOneTray(t *testing.T) {
	calls := recordShell(t)
	b, tr := notificationBackend()
	for _, title := range []string{"one", "two", "three"} {
		if err := b.showNotification(&platform.Notification{ID: title, Title: title}); err != nil {
			t.Fatalf("show %q: %v", title, err)
		}
	}
	if len(b.trays) != 1 {
		t.Fatalf("%d icons for 3 notifications, want the app's one", len(b.trays))
	}
	if tr.notification != "one" {
		t.Errorf("showing %q, want one", tr.notification)
	}
	if want := []string{"two", "three"}; !slices.Equal(b.notificationQueue, want) {
		t.Errorf("queue %v, want %v", b.notificationQueue, want)
	}
	if want := []string{"one"}; !slices.Equal(balloonTitles(*calls), want) {
		t.Errorf("balloons %v, want %v", balloonTitles(*calls), want)
	}
	for _, want := range []string{"two", "three"} {
		b.finishNotification(tr)
		if tr.notification != want {
			t.Fatalf("showing %q, want %q", tr.notification, want)
		}
	}
	b.finishNotification(tr)
	if tr.notification != "" || len(b.notificationQueue) != 0 {
		t.Errorf("after the last balloon: showing %q, queue %v", tr.notification, b.notificationQueue)
	}
	if len(b.trays) != 1 {
		t.Errorf("%d icons left, want the app's one", len(b.trays))
	}
}

// TestNotificationReplacedUnderItsID shows a notification again under its
// ID: one showing takes the new content at once, one waiting keeps its
// place and shows it when its turn comes.
func TestNotificationReplacedUnderItsID(t *testing.T) {
	calls := recordShell(t)
	b, tr := notificationBackend()
	b.showNotification(&platform.Notification{ID: "a", Title: "first"})
	b.showNotification(&platform.Notification{ID: "b", Title: "second"})

	if err := b.showNotification(&platform.Notification{ID: "a", Title: "first again"}); err != nil {
		t.Fatal(err)
	}
	if tr.notification != "a" || !slices.Equal(b.notificationQueue, []string{"b"}) {
		t.Fatalf("after replacing a: showing %q, queue %v", tr.notification, b.notificationQueue)
	}
	if titles := balloonTitles(*calls); titles[len(titles)-1] != "first again" {
		t.Errorf("the balloon shows %q, want first again", titles[len(titles)-1])
	}

	shown := len(*calls)
	if err := b.showNotification(&platform.Notification{ID: "b", Title: "second again"}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != shown {
		t.Error("a notification still waiting showed its balloon at once")
	}
	b.finishNotification(tr)
	if titles := balloonTitles(*calls); titles[len(titles)-1] != "second again" {
		t.Errorf("the next balloon shows %q, want second again", titles[len(titles)-1])
	}
	if len(b.trays) != 1 {
		t.Errorf("%d icons, want the app's one", len(b.trays))
	}
}

// TestNotificationRemoved removes one notification waiting and the one
// showing: the queue goes on, and nothing is left behind.
func TestNotificationRemoved(t *testing.T) {
	recordShell(t)
	b, tr := notificationBackend()
	for _, id := range []string{"a", "b", "c"} {
		b.showNotification(&platform.Notification{ID: id, Title: id})
	}

	b.RemoveNotification("b")
	if !slices.Equal(b.notificationQueue, []string{"c"}) {
		t.Errorf("queue %v, want [c]", b.notificationQueue)
	}
	b.RemoveNotification("a") // the balloon showing
	if tr.notification != "c" {
		t.Errorf("showing %q, want c", tr.notification)
	}
	b.RemoveNotification("c")
	if tr.notification != "" || len(b.notificationQueue) != 0 || len(b.notifications) != 0 {
		t.Errorf("after removing all: showing %q, queue %v, %d notifications", tr.notification, b.notificationQueue, len(b.notifications))
	}
	if len(b.trays) != 1 {
		t.Errorf("%d icons left, want the app's one", len(b.trays))
	}
}

// TestNotificationsRemovedAll clears every notification, the one showing
// and those waiting.
func TestNotificationsRemovedAll(t *testing.T) {
	calls := recordShell(t)
	b, tr := notificationBackend()
	for _, id := range []string{"a", "b", "c"} {
		b.showNotification(&platform.Notification{ID: id, Title: id})
	}

	b.RemoveAllNotifications()
	if tr.notification != "" || len(b.notificationQueue) != 0 || len(b.notifications) != 0 {
		t.Errorf("after clearing: showing %q, queue %v, %d notifications", tr.notification, b.notificationQueue, len(b.notifications))
	}
	last := (*calls)[len(*calls)-1]
	if last.nim != nimModify || last.data.InfoFlags != 0 || syscall.UTF16ToString(last.data.InfoTitle[:]) != "" {
		t.Error("clearing did not hide the balloon")
	}
	if len(b.trays) != 1 {
		t.Errorf("%d icons left, want the app's one", len(b.trays))
	}
}

// TestNotificationShowError reports a balloon the notification area
// refuses, as when Explorer is not running.
func TestNotificationShowError(t *testing.T) {
	old := shellNotifyIcon
	shellNotifyIcon = func(nim uintptr, d *notifyIconData) bool { return nim != nimModify }
	t.Cleanup(func() { shellNotifyIcon = old })
	b, tr := notificationBackend()
	if err := b.showNotification(&platform.Notification{ID: "a", Title: "a"}); err == nil {
		t.Fatal("show succeeded while the notification area refused the balloon")
	}
	if tr.notification != "" || len(b.notificationQueue) != 0 || len(b.notifications) != 0 {
		t.Errorf("after the refused show: showing %q, queue %v, %d notifications", tr.notification, b.notificationQueue, len(b.notifications))
	}
	if len(b.trays) != 1 {
		t.Errorf("%d icons, want the app's one", len(b.trays))
	}
}

// TestNotificationTrayDestroyed drops the notification whose balloon goes
// with an icon, and shows the next one where another icon can.
func TestNotificationTrayDestroyed(t *testing.T) {
	recordShell(t)
	b, tr := notificationBackend()
	b.showNotification(&platform.Notification{ID: "a", Title: "a"})
	b.showNotification(&platform.Notification{ID: "b", Title: "b"})
	tr.Destroy()
	if len(b.trays) != 0 {
		t.Fatalf("%d icons left after destroying the only one, want 0", len(b.trays))
	}
	if b.notifications["a"] != nil {
		t.Error("the notification whose balloon went with the icon is still tracked")
	}
	if b.notifications["b"] == nil || !slices.Equal(b.notificationQueue, []string{"b"}) {
		t.Errorf("the notification waiting was not kept: %+v, queue %v", b.notifications["b"], b.notificationQueue)
	}

	b, first := notificationBackend()
	second := &tray{b: b, id: 2, added: true}
	b.trays[second.id] = second
	b.nextTray = 2
	b.showNotification(&platform.Notification{ID: "a", Title: "a"})
	b.showNotification(&platform.Notification{ID: "b", Title: "b"})
	first.Destroy()
	if second.notification != "b" || len(b.trays) != 1 {
		t.Errorf("after destroying the first icon: showing %q, %d icons, want b on the second", second.notification, len(b.trays))
	}
}

// TestNotificationTemporaryIconReleased lets the app's tray icon take over
// from a temporary one, which goes at once.
func TestNotificationTemporaryIconReleased(t *testing.T) {
	recordShell(t)
	b := New()
	app := &tray{b: b, id: 1, added: false}
	b.trays[app.id] = app
	b.nextTray = 1
	b.showNotification(&platform.Notification{ID: "a", Title: "a"})
	b.showNotification(&platform.Notification{ID: "b", Title: "b"})
	var temp *tray
	for _, tr := range b.trays {
		if tr.temporary {
			temp = tr
		}
	}
	if len(b.trays) != 2 || temp == nil || temp.notification != "a" {
		t.Fatalf("%d icons, temporary %v, want the app's and one showing a", len(b.trays), temp)
	}
	app.added = true
	b.finishNotification(temp)
	if len(b.trays) != 1 || app.notification != "b" {
		t.Errorf("after the app's icon took over: %d icons, showing %q, want 1 showing b", len(b.trays), app.notification)
	}
}

// TestNotificationTemporaryIconReused shows notifications of an app
// without a tray icon on one temporary icon, which goes with the last.
func TestNotificationTemporaryIconReused(t *testing.T) {
	recordShell(t)
	b := New()
	for _, title := range []string{"one", "two", "three"} {
		if err := b.showNotification(&platform.Notification{ID: title, Title: title}); err != nil {
			t.Fatalf("show %q: %v", title, err)
		}
	}
	if len(b.trays) != 1 {
		t.Fatalf("%d icons, want one", len(b.trays))
	}
	var temp *tray
	for _, tr := range b.trays {
		temp = tr
	}
	if !temp.temporary || temp.notification != "one" {
		t.Fatalf("the icon is temporary=%v showing %q, want one", temp.temporary, temp.notification)
	}
	b.finishNotification(temp)
	if len(b.trays) != 1 || temp.notification != "two" {
		t.Fatalf("%d icons, showing %q, want the same icon showing two", len(b.trays), temp.notification)
	}
	b.finishNotification(temp)
	b.finishNotification(temp)
	if len(b.trays) != 0 {
		t.Errorf("%d icons left after the last notification, want 0", len(b.trays))
	}
}
