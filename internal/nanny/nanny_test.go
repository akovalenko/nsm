package nanny

import (
	"testing"
	"time"
)

func TestNextDelay(t *testing.T) {
	o := Options{}
	d := NextDelay(o, 0, time.Second)
	if d != time.Second {
		t.Errorf("first quick failure: %s", d)
	}
	d = NextDelay(o, d, time.Second)
	if d != 2*time.Second {
		t.Errorf("second: %s", d)
	}
	for i := 0; i < 10; i++ {
		d = NextDelay(o, d, 0)
	}
	if d != 60*time.Second {
		t.Errorf("ceiling: %s", d)
	}
	if d = NextDelay(o, d, time.Minute); d != time.Second {
		t.Errorf("healthy run should reset: %s", d)
	}
}
