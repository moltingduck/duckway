package services

import (
	"sync"
	"testing"
	"time"
)

func TestHub_PublishToSubscriber(t *testing.T) {
	h := NewCCEventHub()
	sub, unsub := h.Subscribe("client1")
	defer unsub()

	go h.Publish("client1", CCEvent{Type: "message_create", CCID: "cc1", Handle: "h1"})

	select {
	case ev := <-sub:
		if ev.Type != "message_create" || ev.CCID != "cc1" || ev.Handle != "h1" {
			t.Errorf("got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestHub_NoCrossTalk(t *testing.T) {
	h := NewCCEventHub()
	sub1, unsub1 := h.Subscribe("client1")
	defer unsub1()
	sub2, unsub2 := h.Subscribe("client2")
	defer unsub2()

	h.Publish("client1", CCEvent{Type: "x"})

	select {
	case <-sub1:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("client1 did not receive its event")
	}
	select {
	case ev := <-sub2:
		t.Fatalf("client2 got cross-talk event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHub_MultipleSubscribersPerClient(t *testing.T) {
	h := NewCCEventHub()
	sub1, unsub1 := h.Subscribe("c")
	defer unsub1()
	sub2, unsub2 := h.Subscribe("c")
	defer unsub2()

	h.Publish("c", CCEvent{Type: "x"})

	for _, s := range []<-chan CCEvent{sub1, sub2} {
		select {
		case <-s:
		case <-time.After(time.Second):
			t.Error("a subscriber did not receive the broadcast")
		}
	}
}

func TestHub_DropsWhenBufferFull(t *testing.T) {
	h := NewCCEventHub()
	_, unsub := h.Subscribe("c")
	defer unsub()

	// Buffer is 32; drown it deliberately and confirm Publish doesn't block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish("c", CCEvent{Type: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked under buffer overflow — must be non-blocking")
	}
}

func TestHub_UnsubscribeStopsDelivery(t *testing.T) {
	h := NewCCEventHub()
	sub, unsub := h.Subscribe("c")
	unsub()
	// Channel should be closed.
	if _, ok := <-sub; ok {
		t.Error("expected channel closed after unsubscribe")
	}
	if h.SubscriberCount("c") != 0 {
		t.Errorf("unsub did not remove subscriber")
	}
}

func TestHub_ConcurrentPubSub(t *testing.T) {
	h := NewCCEventHub()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, unsub := h.Subscribe("c")
			defer unsub()
			for j := 0; j < 100; j++ {
				h.Publish("c", CCEvent{Type: "x"})
				select {
				case <-sub:
				case <-time.After(time.Second):
					return
				}
			}
		}()
	}
	wg.Wait()
}
