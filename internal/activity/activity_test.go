package activity

import "testing"

func TestRingKeepsNewestFirst(t *testing.T) {
	l := New(3)
	for i := 1; i <= 5; i++ {
		l.Add(Entry{Number: i})
	}
	got := l.List()
	if len(got) != 3 || got[0].Number != 5 || got[2].Number != 3 {
		t.Fatalf("got %+v", got)
	}
	var nilLog *Log
	nilLog.Add(Entry{}) // must not panic
}
