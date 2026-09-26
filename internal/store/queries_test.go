package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
)

func TestMeterKeys(t *testing.T) {
	s, _ := openTemp(t)
	a := mustAccount(t, s, domain.ProviderClaude, "aaaa")
	b := mustAccount(t, s, domain.ProviderCodex, "bbbb")
	ev := event(a.ID, "k1", base)
	if _, err := s.InsertEvent(&ev, []domain.MeterReading{reading("7d", base, 0.1), reading("5h", base, 0.2)}); err != nil {
		t.Fatal(err)
	}
	poll := reading("7d_oi", base, 0.3)
	poll.AccountID, poll.Source = a.ID, domain.SourcePoll
	if err := s.InsertReading(&poll); err != nil {
		t.Fatal(err)
	}
	other := reading("7d", base, 0.4)
	other.AccountID = b.ID
	if err := s.InsertReading(&other); err != nil {
		t.Fatal(err)
	}
	got, err := s.MeterKeys(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"5h", "7d", "7d_oi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MeterKeys(a) = %v, want %v", got, want)
	}
	if got, _ := s.MeterKeys(b.ID); !reflect.DeepEqual(got, []string{"7d"}) {
		t.Errorf("MeterKeys(b) = %v", got)
	}
	if got, _ := s.MeterKeys(999); got != nil {
		t.Errorf("MeterKeys(unknown) = %v", got)
	}
}

func TestListEventsNoHeaders(t *testing.T) {
	s, _ := openTemp(t)
	a := mustAccount(t, s, domain.ProviderClaude, "aaaa")
	for i, key := range []string{"k1", "k2", "k3"} {
		ev := event(a.ID, key, base.Add(time.Duration(i)*time.Minute))
		if _, err := s.InsertEvent(&ev, nil); err != nil {
			t.Fatal(err)
		}
	}
	q := EventQuery{AccountID: a.ID, From: base.Add(time.Minute), Limit: 10}
	full, err := s.ListEvents(q)
	if err != nil {
		t.Fatal(err)
	}
	slim, err := s.ListEventsNoHeaders(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(slim) != 2 || len(full) != 2 {
		t.Fatalf("got %d slim, %d full events", len(slim), len(full))
	}
	for i := range slim {
		if slim[i].HeadersJSON != nil {
			t.Errorf("slim event %d carries headers", slim[i].ID)
		}
		full[i].HeadersJSON = nil
		if !reflect.DeepEqual(slim[i], full[i]) {
			t.Errorf("slim %+v != full %+v", slim[i], full[i])
		}
	}
	desc, _ := s.ListEventsNoHeaders(EventQuery{AccountID: a.ID, Desc: true, Limit: 1})
	if len(desc) != 1 || desc[0].DedupKey != "k3" {
		t.Errorf("desc = %+v", desc)
	}
}
