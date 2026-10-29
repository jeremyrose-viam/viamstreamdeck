package viamstreamdeck

import (
	"context"
	"sync"
	"testing"

	"go.viam.com/test"
)

// The component redraws every key once a second while update_display and
// Reconfigure run on other goroutines. Run under -race.
func TestRedrawLoopDoesNotRaceWithReconfigureAndUpdates(t *testing.T) {
	sdc, _ := newTestComponent(t, flatConfig(), "thing")
	ctx := context.Background()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = sdc.redraw(ctx) // what stateChecker does each tick
			}
		}
	}()

	for i := 0; i < 200; i++ {
		_, err := update(sdc, keyUpdate("0", map[string]interface{}{"text": "x"}))
		test.That(t, err, test.ShouldBeNil)
		fresh := flatConfig()
		_, _, err = fresh.Validate("")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, sdc.reconfigure(ctx, sdc.deps, fresh), test.ShouldBeNil)
	}
	close(done)
	wg.Wait()
}
