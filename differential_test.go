package viamstreamdeck

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"go.viam.com/test"
)

// keysString renders every pre-existing KeyConfig field, so the trace compares
// the same thing before and after the new optional Name field.
func keysString(keys []KeyConfig) string {
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("{%d text=%q text_color=%q color=%q image=%q component=%q method=%q args=%v}",
			k.Key, k.Text, k.TextColor, k.Color, k.Image, k.Component, k.Method, k.Args))
	}
	return strings.Join(parts, " ")
}

// TestFlatDifferentialTrace replays a scripted session against a flat config
// and compares everything observable (each response or error, every call made
// to the deck, and the resulting config) with testdata/flat_trace.golden. The
// golden file was recorded from the code before pages support, so a pass shows
// flat configs behave identically. The one intentional difference, a malformed
// key reference like "1abc", and the reworded error for a non-numeric one like
// "abc", are deliberately left out of the script; paged_test.go and
// flat_baseline_test.go cover them.
//
// Regenerate with UPDATE_GOLDEN=1 only for an intentional behavior change.
func TestFlatDifferentialTrace(t *testing.T) {
	conf := &Config{
		Brightness: 50,
		Keys: []KeyConfig{
			textKey(0, "zero"),
			{Key: 1, Text: "one", Color: "red", TextColor: "white", Component: "thing", Method: "do_command"},
			{Key: 2, Image: "x.jpg", Component: "thing", Method: "do_command"},
			{Key: 3, Text: "lost", Component: "gone", Method: "do_command"},
		},
		Dials: []DialConfig{{Dial: 0, Component: "thing", Command: "DoCommand"}},
	}
	sdc, fd := newTestComponent(t, conf, "thing")
	ctx := context.Background()

	var trace strings.Builder
	mark := 0
	step := func(name string, res map[string]interface{}, err error) {
		fmt.Fprintf(&trace, "## %s\n", name)
		if err != nil {
			// Some errors print a whole KeyConfig with %v; collapsing runs of
			// spaces keeps the new Name field's empty column from showing up.
			fmt.Fprintf(&trace, "error: %s\n", strings.Join(strings.Fields(err.Error()), " "))
		} else {
			fmt.Fprintf(&trace, "result: %v\n", res)
		}
		for _, op := range fd.ops[mark:] {
			fmt.Fprintf(&trace, "  %s\n", op)
		}
		mark = len(fd.ops)
		fmt.Fprintf(&trace, "keys: %s\ndials: %+v\nbrightness: %d\n\n", keysString(sdc.conf.Keys), sdc.conf.Dials, sdc.conf.Brightness)
	}
	cmd := func(c map[string]interface{}) (map[string]interface{}, error) { return sdc.DoCommand(ctx, c) }
	up := func(body map[string]interface{}) (map[string]interface{}, error) { return update(sdc, body) }
	k := func(ref string, f map[string]interface{}) map[string]interface{} { return keyUpdate(ref, f) }

	step("initial draw", nil, nil)

	r, err := up(k("0", map[string]interface{}{"text": "ZERO"}))
	step("edit text", r, err)
	r, err = up(k("1", map[string]interface{}{"color": "blue", "text_color": "yellow"}))
	step("edit colors", r, err)
	r, err = up(k("2", map[string]interface{}{"text": "caption"}))
	step("add text to an image key", r, err)
	r, err = up(k("3", map[string]interface{}{"component": "thing"}))
	step("point a missing-component key at a real one", r, err)
	r, err = up(k("4", map[string]interface{}{"text": "new", "component": "thing", "method": "do_command", "args": []interface{}{map[string]interface{}{"a": 1}}}))
	step("add a key", r, err)
	r, err = up(k("5", map[string]interface{}{"text": "orphan", "component": "nowhere", "method": "do_command"}))
	step("add a key for a missing component", r, err)
	r, err = up(map[string]interface{}{"brightness": 70})
	step("brightness", r, err)
	r, err = up(map[string]interface{}{"dials": map[string]interface{}{"1": map[string]interface{}{"component": "thing", "command": "SetPosition"}}})
	step("add dial", r, err)
	r, err = up(map[string]interface{}{"dials": map[string]interface{}{"0": map[string]interface{}{"command": "SetPosition"}}})
	step("edit dial", r, err)
	r, err = up(map[string]interface{}{"brightness": 20, "keys": map[string]interface{}{"0": map[string]interface{}{"text": "both"}}})
	step("brightness and a key", r, err)

	r, err = up(k("0", map[string]interface{}{"method": "get_readings"}))
	step("reject unsupported method", r, err)
	r, err = up(k("0", map[string]interface{}{"image": "nope.png"}))
	step("reject unknown image", r, err)
	r, err = up(k("9", map[string]interface{}{"component": "thing", "method": "do_command"}))
	step("reject a new key with nothing to show", r, err)
	r, err = up(map[string]interface{}{})
	step("reject empty update", r, err)
	r, err = cmd(map[string]interface{}{"set_page": "p"})
	step("set_page on a flat config", r, err)
	r, err = cmd(map[string]interface{}{"nope": 1})
	step("unknown command", r, err)

	err = sdc.reconfigure(ctx, sdc.deps, sdc.conf)
	step("redraw tick", nil, err)
	r, err = up(k("1", map[string]interface{}{"text": "after tick"}))
	step("edit after a tick", r, err)

	const golden = "testdata/flat_trace.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		test.That(t, os.MkdirAll("testdata", 0o755), test.ShouldBeNil)
		test.That(t, os.WriteFile(golden, []byte(trace.String()), 0o644), test.ShouldBeNil)
		return
	}
	want, err := os.ReadFile(golden)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, trace.String(), test.ShouldEqual, string(want))
}
