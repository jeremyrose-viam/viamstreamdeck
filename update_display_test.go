package viamstreamdeck

import (
	"context"
	"errors"
	"testing"

	"go.viam.com/test"
)

func pagesConfig() *Config {
	return &Config{
		InitialPage: "main",
		Pages: map[string][]KeyConfig{
			"main": {
				{Key: 0, Name: "go-refill", Text: "Refill", Color: "blue", Component: "stream-deck", Method: "do_command"},
			},
			"Refill": {
				{Key: 1, Name: "water", Text: "Water", Color: "teal", Component: "usage-sensor", Method: "do_command"},
				{Key: 2, Name: "beans", Text: "Beans", Color: "teal", Component: "usage-sensor", Method: "do_command"},
			},
		},
	}
}

func flatConfig() *Config {
	return &Config{
		Keys: []KeyConfig{
			{Key: 0, Text: "zero", Color: "blue", Component: "stream-deck", Method: "do_command"},
			{Key: 1, Name: "one", Text: "one", Color: "teal", Component: "stream-deck", Method: "do_command"},
		},
	}
}

// drawn records what the component asked the deck to draw.
type drawn struct {
	keys []KeyConfig
	err  error
}

func (d *drawn) draw(_ context.Context, k KeyConfig) error {
	if d.err != nil {
		return d.err
	}
	d.keys = append(d.keys, k)
	return nil
}

func newTestComponent(conf *Config, currentPage string) (*streamdeckComponent, *drawn) {
	d := &drawn{}
	return &streamdeckComponent{
		conf:        conf,
		keys:        map[int]KeyConfig{},
		currentPage: currentPage,
		draw:        d.draw,
	}, d
}

func TestLocateKey(t *testing.T) {
	keys := pagesConfig().Pages["Refill"]

	pos, num, err := locateKey(keys, "beans")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pos, test.ShouldEqual, 1)
	test.That(t, num, test.ShouldEqual, 2)

	pos, num, err = locateKey(keys, "1")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pos, test.ShouldEqual, 0)
	test.That(t, num, test.ShouldEqual, 1)

	// An index with no entry yet is addressable, so a key can be added.
	pos, num, err = locateKey(keys, "7")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pos, test.ShouldEqual, -1)
	test.That(t, num, test.ShouldEqual, 7)

	_, _, err = locateKey(keys, "ice")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, `no key named "ice"`)

	// A name used twice can't be addressed by name.
	dup := []KeyConfig{{Key: 3, Name: "back"}, {Key: 4, Name: "back"}}
	_, _, err = locateKey(dup, "back")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "more than one key")
	// ...but each is still reachable by number.
	pos, num, err = locateKey(dup, "4")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pos, test.ShouldEqual, 1)
	test.That(t, num, test.ShouldEqual, 4)

	// A name that looks like a number is read as a number.
	numeric := []KeyConfig{{Key: 9, Name: "3"}}
	pos, num, err = locateKey(numeric, "3")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pos, test.ShouldEqual, -1)
	test.That(t, num, test.ShouldEqual, 3)

	// Junk that Sscanf("%d") used to accept as a number is no longer one.
	_, _, err = locateKey(keys, "4abc")
	test.That(t, err, test.ShouldNotBeNil)
}

// Key names must never stop a config that loaded before from loading: "name"
// was an ignored field, so existing configs may already carry duplicate or
// numeric values there.
func TestNamesNeverFailValidation(t *testing.T) {
	conf := pagesConfig()
	_, _, err := conf.Validate("")
	test.That(t, err, test.ShouldBeNil)

	dup := pagesConfig()
	dup.Pages["Refill"][1].Name = "water"
	_, _, err = dup.Validate("")
	test.That(t, err, test.ShouldBeNil)

	numeric := pagesConfig()
	numeric.Pages["Refill"][0].Name = "3"
	_, _, err = numeric.Validate("")
	test.That(t, err, test.ShouldBeNil)

	flat := flatConfig()
	flat.Keys[0].Name = "one"
	_, _, err = flat.Validate("")
	test.That(t, err, test.ShouldBeNil)
}

// Updating a page that is not showing only edits that page's config; it never
// reaches the device.
func TestUpdateDisplayKeyOtherPage(t *testing.T) {
	sdc, d := newTestComponent(pagesConfig(), "main")

	n, err := sdc.updateDisplayKey(context.Background(), "Refill", "beans", map[string]interface{}{"color": "red", "text": "Beans 12"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 2)

	beans := sdc.conf.Pages["Refill"][1]
	test.That(t, beans.Color, test.ShouldEqual, "red")
	test.That(t, beans.Text, test.ShouldEqual, "Beans 12")
	test.That(t, beans.Name, test.ShouldEqual, "beans")

	// Everything else is untouched, nothing was drawn, and pages mode is kept.
	test.That(t, sdc.conf.Pages["Refill"][0].Color, test.ShouldEqual, "teal")
	test.That(t, sdc.conf.Pages["main"][0].Color, test.ShouldEqual, "blue")
	test.That(t, len(sdc.conf.Keys), test.ShouldEqual, 0)
	test.That(t, len(sdc.keys), test.ShouldEqual, 0)
	test.That(t, len(d.keys), test.ShouldEqual, 0)

	// A key can't be added to a page that isn't showing; the page is unchanged.
	_, err = sdc.updateDisplayKey(context.Background(), "Refill", "5", map[string]interface{}{"text": "New", "color": "green"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, len(sdc.conf.Pages["Refill"]), test.ShouldEqual, 2)
	test.That(t, len(sdc.conf.Pages["main"]), test.ShouldEqual, 1)
	test.That(t, len(sdc.conf.Keys), test.ShouldEqual, 0)
}

// A bad update to a page that isn't showing must be rejected when it is sent.
// It isn't drawn, so if it were saved it would sit in the config and break the
// page, and the keys after it, the next time someone navigated there.
func TestUpdateDisplayKeyHiddenPageRejectsUndrawableKeys(t *testing.T) {
	ctx := context.Background()

	bad := map[string]map[string]interface{}{
		"unsupported method": {"method": "bogus"},
		"no method":          {"method": ""},
		"no component":       {"component": ""},
		"unknown image":      {"image": "no-such-image.png"},
	}
	for name, updates := range bad {
		t.Run(name, func(t *testing.T) {
			sdc, d := newTestComponent(pagesConfig(), "main")
			before := sdc.conf.Pages["Refill"][0]

			_, err := sdc.updateDisplayKey(ctx, "Refill", "water", updates)
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, sdc.conf.Pages["Refill"][0], test.ShouldResemble, before)
			test.That(t, len(d.keys), test.ShouldEqual, 0)
		})
	}

	// New keys can't be created on a page that isn't showing.
	sdc, _ := newTestComponent(pagesConfig(), "main")
	_, err := sdc.updateDisplayKey(ctx, "Refill", "9", map[string]interface{}{"text": "new", "component": "usage-sensor", "method": "do_command"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "can only be added to the page that is showing")
	test.That(t, len(sdc.conf.Pages["Refill"]), test.ShouldEqual, 2)

	// A good update to an existing key still goes through.
	_, err = sdc.updateDisplayKey(ctx, "Refill", "water", map[string]interface{}{"text": "Water 5", "color": "red"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, sdc.conf.Pages["Refill"][0].Text, test.ShouldEqual, "Water 5")
}

// A key added to the page that is showing is checked before it is drawn or
// saved, and a complete one is drawn.
func TestUpdateDisplayKeyShowingPageNewKey(t *testing.T) {
	ctx := context.Background()
	sdc, d := newTestComponent(pagesConfig(), "Refill")

	_, err := sdc.updateDisplayKey(ctx, "", "7", map[string]interface{}{"text": "no method"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, len(d.keys), test.ShouldEqual, 0)
	test.That(t, len(sdc.conf.Pages["Refill"]), test.ShouldEqual, 2)

	n, err := sdc.updateDisplayKey(ctx, "", "7", map[string]interface{}{
		"text": "ok", "color": "green", "component": "usage-sensor", "method": "do_command",
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 7)
	test.That(t, len(d.keys), test.ShouldEqual, 1)
	test.That(t, len(sdc.conf.Pages["Refill"]), test.ShouldEqual, 3)
}

// Updating the page that is showing draws the key, records it, and still leaves
// the flat keys list alone, which is what keeps pages mode working.
func TestUpdateDisplayKeyShowingPage(t *testing.T) {
	sdc, d := newTestComponent(pagesConfig(), "Refill")

	// No page given: the page that is showing, addressed by name.
	n, err := sdc.updateDisplayKey(context.Background(), "", "water", map[string]interface{}{"color": "red", "text": "Water 25"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 1)
	test.That(t, len(d.keys), test.ShouldEqual, 1)
	test.That(t, d.keys[0].Text, test.ShouldEqual, "Water 25")
	test.That(t, d.keys[0].Component, test.ShouldEqual, "usage-sensor") // merged, not replaced
	test.That(t, sdc.keys[1].Color, test.ShouldEqual, "red")
	test.That(t, sdc.conf.Pages["Refill"][0].Color, test.ShouldEqual, "red")
	test.That(t, len(sdc.conf.Keys), test.ShouldEqual, 0)

	// The same page named explicitly behaves the same.
	_, err = sdc.updateDisplayKey(context.Background(), "Refill", "2", map[string]interface{}{"text": "Beans 3"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(d.keys), test.ShouldEqual, 2)

	// A draw failure is reported and nothing is saved.
	d.err = errors.New("deck unplugged")
	_, err = sdc.updateDisplayKey(context.Background(), "", "water", map[string]interface{}{"text": "lost"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, sdc.conf.Pages["Refill"][0].Text, test.ShouldEqual, "Water 25")
}

func TestUpdateDisplayKeyErrors(t *testing.T) {
	sdc, _ := newTestComponent(pagesConfig(), "main")

	_, err := sdc.updateDisplayKey(context.Background(), "nope", "1", nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, `page "nope" not found`)

	_, err = sdc.updateDisplayKey(context.Background(), "Refill", "ice", nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, `no key named "ice"`)
}

// A config with a flat keys list takes the path it always did: the key is
// merged, drawn, and saved in the keys list; there are no pages to consider.
func TestUpdateDisplayKeyFlatConfig(t *testing.T) {
	sdc, d := newTestComponent(flatConfig(), "")
	sdc.keys[0] = sdc.conf.Keys[0]
	sdc.keys[1] = sdc.conf.Keys[1]
	ctx := context.Background()

	// An existing key, by number: merged over what's there.
	n, err := sdc.updateDisplayKey(ctx, "", "0", map[string]interface{}{"color": "red"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 0)
	test.That(t, d.keys[0].Text, test.ShouldEqual, "zero")
	test.That(t, d.keys[0].Color, test.ShouldEqual, "red")
	test.That(t, sdc.conf.Keys[0].Color, test.ShouldEqual, "red")
	test.That(t, len(sdc.conf.Keys), test.ShouldEqual, 2)

	// By name (new, and only if one was given).
	n, err = sdc.updateDisplayKey(ctx, "", "one", map[string]interface{}{"text": "uno"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 1)
	test.That(t, sdc.conf.Keys[1].Text, test.ShouldEqual, "uno")

	// A key that isn't in the list yet is appended, as before.
	n, err = sdc.updateDisplayKey(ctx, "", "5", map[string]interface{}{"text": "five", "color": "green", "component": "stream-deck", "method": "do_command"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 5)
	test.That(t, len(sdc.conf.Keys), test.ShouldEqual, 3)
	test.That(t, sdc.keys[5].Text, test.ShouldEqual, "five")

	// A page is ignored, as it was before update_display knew about pages.
	n, err = sdc.updateDisplayKey(ctx, "Refill", "0", map[string]interface{}{"text": "still zero"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 0)
	test.That(t, sdc.conf.Keys[0].Text, test.ShouldEqual, "still zero")
	test.That(t, len(sdc.conf.Pages), test.ShouldEqual, 0)

	// A draw failure is reported and nothing is saved.
	d.err = errors.New("deck unplugged")
	_, err = sdc.updateDisplayKey(ctx, "", "0", map[string]interface{}{"text": "lost"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, sdc.conf.Keys[0].Text, test.ShouldEqual, "still zero")
}
