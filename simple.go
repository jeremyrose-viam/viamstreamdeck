package viamstreamdeck

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/multierr"

	"github.com/mitchellh/mapstructure"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/dh1tw/streamdeck"

	"github.com/erh/vmodutils"

	_ "go.viam.com/rdk/components/arm"
	_ "go.viam.com/rdk/components/base"
	_ "go.viam.com/rdk/components/board"
	_ "go.viam.com/rdk/components/button"
	_ "go.viam.com/rdk/components/camera"
	_ "go.viam.com/rdk/components/generic"
	_ "go.viam.com/rdk/components/gripper"
	_ "go.viam.com/rdk/components/motor"
	_ "go.viam.com/rdk/components/movementsensor"
	_ "go.viam.com/rdk/components/sensor"
	_ "go.viam.com/rdk/services/generic"
	_ "go.viam.com/rdk/services/motion"
	_ "go.viam.com/rdk/services/vision"
)

func NewStreamDeck(ctx context.Context, name resource.Name, deps resource.Dependencies, ms *ModelSetup, conf *Config, logger logging.Logger) (resource.Resource, error) {

	_, _, err := conf.Validate("")
	if err != nil {
		return nil, err
	}

	if ms == nil {
		ms = FindAttachedStreamDeck()
		if ms == nil {
			return nil, fmt.Errorf("no streamdeck found")
		}
	}

	sdc := &streamdeckComponent{
		name:   name,
		logger: logger,
		ms:     ms,
		conf:   conf,
		deps:   deps,
		keys:   map[int]KeyConfig{},
	}

	sdc.sd, err = streamdeck.NewStreamDeckWithConfig(&ms.Conf, "")
	if err != nil && ms == ModelOriginal {
		// original vs original2 is confusing, try it
		ms = ModelOriginal2
		sdc.ms = ModelOriginal2
		sdc.sd, err = streamdeck.NewStreamDeckWithConfig(&ms.Conf, "")
	}

	if err != nil {
		return nil, err
	}

	err = sdc.updateBrightness(conf.Brightness)
	if err != nil {
		return nil, err
	}

	// Initialize with appropriate keys
	if len(conf.Pages) > 0 {
		sdc.currentPage = conf.InitialPage
		logger.Debugf("Initializing with page: %s", sdc.currentPage)
	}

	err = sdc.updateKeys(ctx)
	if err != nil {
		return nil, err
	}

	sdc.sd.SetBtnEventCb(func(s streamdeck.State, e streamdeck.Event) {
		logger.Infof("got event %v", e)
		err := sdc.HandleEvent(context.Background(), s, e)
		if err != nil {
			logger.Errorf("event handler failed for event %v: %v", e, err)
		}
	})

	go sdc.stateChecker()

	return sdc, nil
}

func (sdc *streamdeckComponent) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	newConf, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}

	return sdc.reconfigure(ctx, deps, newConf)
}

func (sdc *streamdeckComponent) reconfigure(ctx context.Context, deps resource.Dependencies, newConf *Config) error {
	sdc.configLock.Lock()
	defer sdc.configLock.Unlock()

	sdc.deps = deps
	sdc.conf = newConf

	err := sdc.updateBrightness(newConf.Brightness)
	if err != nil {
		return err
	}

	return sdc.updateKeys(ctx)
}

type streamdeckComponent struct {
	name   resource.Name
	logger logging.Logger
	ms     *ModelSetup

	sd *streamdeck.StreamDeck

	configLock sync.Mutex
	deps       resource.Dependencies
	conf       *Config
	keys       map[int]KeyConfig

	currentPage string

	closed atomic.Int32

	// draw, if set, replaces updateKey in drawKey. Only tests set it.
	draw func(context.Context, KeyConfig) error
}

func (sdc *streamdeckComponent) updateBrightness(level int) error {
	if level <= 0 {
		return nil
	}
	if level > 100 {
		level = 100
	}
	return sdc.sd.SetBrightness(uint16(level))
}

// applyKeyUpdate merges updates into an existing key config and returns the result
func (sdc *streamdeckComponent) applyKeyUpdate(existing KeyConfig, updates map[string]interface{}) (KeyConfig, error) {
	result := existing

	if text, ok := updates["text"].(string); ok {
		result.Text = text
	}
	if textColor, ok := updates["text_color"].(string); ok {
		result.TextColor = textColor
	}
	if color, ok := updates["color"].(string); ok {
		result.Color = color
	}
	if image, ok := updates["image"].(string); ok {
		result.Image = image
	}
	if component, ok := updates["component"].(string); ok {
		result.Component = component
	}
	if method, ok := updates["method"].(string); ok {
		result.Method = method
	}
	if args, ok := updates["args"].([]interface{}); ok {
		result.Args = args
	}

	return result, nil
}

// applyDialUpdate merges updates into an existing dial config and returns the result
func (sdc *streamdeckComponent) applyDialUpdate(existing DialConfig, updates map[string]interface{}) (DialConfig, error) {
	result := existing

	if component, ok := updates["component"].(string); ok {
		result.Component = component
	}
	if command, ok := updates["command"].(string); ok {
		result.Command = command
	}

	return result, nil
}

func (sdc *streamdeckComponent) isSelfReference(componentName string) bool {
	return componentName == sdc.name.ShortName()
}

func (sdc *streamdeckComponent) updateKey(ctx context.Context, k KeyConfig) error {
	_, ok := vmodutils.FindDep(sdc.deps, k.Component)
	if !ok && !sdc.isSelfReference(k.Component) {
		sdc.logger.Warnf("missing component %v deps: %v", k.Component, sdc.deps)

		img, ok := assetImages["x.jpg"]
		if !ok {
			return fmt.Errorf("can't find dependency %s nore, the x image :(", k.Component)
		}

		return sdc.sd.WriteTextOnImage(
			k.Key,
			img,
			[]streamdeck.TextLine{{Text: k.Component, PosX: 10, PosY: 30, FontSize: 20, FontColor: getColor("black", "black")}},
		)
	}

	if snakeToCamel(k.Method) != "DoCommand" && snakeToCamel(k.Method) != "SetPosition" {
		return fmt.Errorf("only support DoCommand and SetPosition now, not %s", k.Method)
	}

	if k.Image != "" {
		img, ok := assetImages[k.Image]
		if ok {
			if k.Text != "" {
				return sdc.sd.WriteTextOnImage(
					k.Key,
					img,
					sdc.ms.SimpleText(k.Text, k.TextColor, k.TextFont),
				)
			}
			return sdc.sd.FillImage(k.Key, img)
		}
		return fmt.Errorf("unknown image [%s]", k.Image)
	}

	if k.Text == "" && snakeToCamel(k.Method) == "SetPosition" {
		s, err := sdc.findSwitch(ctx, k.Component)
		if err != nil {
			return err
		}
		_, names, err := s.GetNumberOfPositions(ctx, nil)
		if err != nil {
			return err
		}

		n, err := sdc.findSwitchArg(k)
		if err != nil {
			return err
		}

		if n < 0 || int(n) >= len(names) {
			return fmt.Errorf("invalid position %d", n)
		}

		if k.Color == "" && k.TextColor == "" {
			pos, err := s.GetPosition(ctx, nil)
			if err != nil {
				return err
			}

			if pos == n {
				k.TextColor = "black"
				k.Color = "white"
			} else {
				k.TextColor = "white"
				k.Color = "black"
			}
		}

		k.Text = names[n]
	}

	if k.Text != "" {
		return sdc.sd.WriteText(k.Key, sdc.ms.SimpleTextButton(k.Text, k.Color, k.TextColor, k.TextFont))
	}

	return fmt.Errorf("nothing to display for key %v", k)
}

// applyKeys renders the given keys on the Stream Deck, clearing any
// previously displayed keys that aren't in the new set.
func (sdc *streamdeckComponent) applyKeys(ctx context.Context, keys []KeyConfig) error {
	newKeyIndices := make(map[int]bool)
	for _, k := range keys {
		newKeyIndices[k.Key] = true
	}

	// Clear any keys that are currently configured but not in the new set
	for keyIdx := range sdc.keys {
		if !newKeyIndices[keyIdx] {
			// Clear this key from the display
			err := sdc.sd.ClearBtn(keyIdx)
			if err != nil {
				sdc.logger.Errorf("failed to clear key %d: %v", keyIdx, err)
			}
			// Remove from internal cache
			delete(sdc.keys, keyIdx)
		}
	}

	// Apply all the new keys
	for _, k := range keys {
		err := sdc.updateKey(ctx, k)
		if err != nil {
			return err
		}
		sdc.keys[k.Key] = k
	}
	return nil
}

// updateKeys resolves which keys should be displayed based on the current
// config (keys vs pages) and applies them.
func (sdc *streamdeckComponent) updateKeys(ctx context.Context) error {
	var keysToLoad []KeyConfig
	if len(sdc.conf.Keys) > 0 {
		keysToLoad = sdc.conf.Keys
		sdc.currentPage = ""
	} else if len(sdc.conf.Pages) > 0 {
		if sdc.currentPage != "" {
			var err error
			keysToLoad, err = sdc.conf.GetKeysForPage(sdc.currentPage)
			if err != nil {
				// Current page no longer exists, switch to initial_page
				sdc.currentPage = sdc.conf.InitialPage
				keysToLoad, _ = sdc.conf.GetKeysForPage(sdc.currentPage)
			}
		} else {
			// No current page set, use initial_page
			sdc.currentPage = sdc.conf.InitialPage
			keysToLoad, _ = sdc.conf.GetKeysForPage(sdc.currentPage)
		}
	}

	return sdc.applyKeys(ctx, keysToLoad)
}

func (sdc *streamdeckComponent) findSwitchArg(k KeyConfig) (uint32, error) {
	return switchArg(k)
}

func switchArg(k KeyConfig) (uint32, error) {
	if len(k.Args) == 1 {
		switch v := k.Args[0].(type) {
		case int:
			return uint32(v), nil
		case float64:
			return uint32(v), nil
		case int32:
			return uint32(v), nil
		}
	}
	return 0, fmt.Errorf("need 1 number arg, got: %v", k.Args)
}

func (sdc *streamdeckComponent) findSwitch(ctx context.Context, name string) (toggleswitch.Switch, error) {
	r, ok := vmodutils.FindDep(sdc.deps, name)
	if !ok {
		return nil, fmt.Errorf("no resource %s", name)
	}

	s, ok := r.(toggleswitch.Switch)
	if !ok {
		return nil, fmt.Errorf("%s is a %T not switch", name, r)
	}

	return s, nil

}

func (sdc *streamdeckComponent) getKeyConfig(which int) (*KeyConfig, error) {
	sdc.configLock.Lock()
	defer sdc.configLock.Unlock()

	k, ok := sdc.keys[which]
	if !ok {
		return nil, fmt.Errorf("no key for %v", which)
	}
	return &k, nil
}

func (sdc *streamdeckComponent) getResourceAndCommandForKey(which int, e streamdeck.Event) (resource.Resource, map[string]interface{}, error) {
	sdc.configLock.Lock()
	defer sdc.configLock.Unlock()

	k, ok := sdc.keys[which]
	if !ok {
		return nil, nil, fmt.Errorf("no key for %v", e)
	}

	var r resource.Resource
	// Check if this is a self-reference
	if sdc.isSelfReference(k.Component) {
		r = sdc
	} else {
		r, ok = vmodutils.FindDep(sdc.deps, k.Component)
		if !ok {
			return nil, nil, fmt.Errorf("no resource %s for %s", k.Component, e)
		}
	}

	cmd := map[string]interface{}{}

	if len(k.Args) > 0 {
		cmd, ok = k.Args[0].(map[string]interface{})
		if !ok {
			return nil, nil, fmt.Errorf("args wrong for %v %v %T", e, k.Args[0], k.Args[0])
		}
	}

	return r, cmd, nil
}

func (sdc *streamdeckComponent) getResourceAndCommandForDial(which int) (resource.Resource, string, error) {
	sdc.configLock.Lock()
	defer sdc.configLock.Unlock()

	for _, dc := range sdc.conf.Dials {
		if which != dc.Dial {
			continue
		}

		var r resource.Resource
		var ok bool
		// Check if this is a self-reference
		if sdc.isSelfReference(dc.Component) {
			r = sdc
		} else {
			r, ok = vmodutils.FindDep(sdc.deps, dc.Component)
			if !ok {
				return nil, "", fmt.Errorf("no resource %s", dc.Component)
			}
		}

		return r, dc.Command, nil
	}

	return nil, "", fmt.Errorf("no config for dial %d", which)
}

func (sdc *streamdeckComponent) handleKeyPress(ctx context.Context, s streamdeck.State, e streamdeck.Event, which int) error {
	k, err := sdc.getKeyConfig(which)
	if err != nil {
		return err
	}

	if k.snakeMethod() == "DoCommand" {
		r, cmd, err := sdc.getResourceAndCommandForKey(which, e)
		if err != nil {
			return err
		}

		res, err := r.DoCommand(ctx, cmd)
		if err != nil {
			return err
		}
		sdc.logger.Infof("event %v got result %v", e, res)
		return nil
	} else if k.snakeMethod() == "SetPosition" {
		s, err := sdc.findSwitch(ctx, k.Component)
		if err != nil {
			return err
		}

		n, err := sdc.findSwitchArg(*k)
		if err != nil {
			return err
		}

		return s.SetPosition(ctx, n, nil)

	} else {
		return fmt.Errorf("can't handle command %v", k.snakeMethod())
	}
}

func (sdc *streamdeckComponent) handleDialTurn(ctx context.Context, s streamdeck.State, which int) error {
	sdc.logger.Infof("handleDialTurn called which: %v state: %v", which, s.DialPos[which])
	r, c, err := sdc.getResourceAndCommandForDial(which)
	if err != nil {
		return err
	}

	switch c {
	case "DoCommand":
		res, err := r.DoCommand(ctx, map[string]any{c: float64(s.DialPos[which])})
		if err != nil {
			return err
		}
		sdc.logger.Infof("res: %v", res)
		return nil
	case "SetPosition":
		sw, ok := r.(toggleswitch.Switch)
		if !ok {
			return fmt.Errorf("resource %v is not a switch", r)
		}
		return sw.SetPosition(ctx, uint32(s.DialPos[which]), nil)
	}

	return fmt.Errorf("can't handle command %v", c)
}

func (sdc *streamdeckComponent) HandleEvent(ctx context.Context, s streamdeck.State, e streamdeck.Event) error {
	sdc.logger.Infof("got event %v", e)

	switch e.Kind {
	case streamdeck.EventKeyPressed:
		return nil
	case streamdeck.EventKeyReleased:
		return sdc.handleKeyPress(ctx, s, e, e.Which)
	case streamdeck.EventDialTurn:
		return sdc.handleDialTurn(ctx, s, e.Which)
	}

	return fmt.Errorf("HandleEvent for %v not done", e)
}

func (sdc *streamdeckComponent) Name() resource.Name {
	return sdc.name
}

func (sdc *streamdeckComponent) stateChecker() {
	for sdc.closed.Load() == 0 {

		err := sdc.reconfigure(context.Background(), sdc.deps, sdc.conf)
		if err != nil {
			sdc.logger.Errorf("can't reconfigure: %v", err)
		}

		time.Sleep(time.Second)
	}
}

func (sdc *streamdeckComponent) Close(ctx context.Context) error {
	sdc.closed.Store(1)
	return multierr.Combine(sdc.sd.ClearAllBtns(), sdc.sd.Close())
}

func (sdc *streamdeckComponent) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (sdc *streamdeckComponent) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	// Check for set_page command
	if pageName, ok := cmd["set_page"].(string); ok {
		err := sdc.setPage(ctx, pageName)
		if err != nil {
			return nil, err
		}

		return map[string]interface{}{
			"success": true,
			"page":    pageName,
		}, nil
	}

	if updateData, ok := cmd["update_display"]; ok {
		updateMap, ok := updateData.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("update_display must be an object/map")
		}
		return sdc.handleUpdateDisplay(ctx, updateMap)
	}

	return nil, fmt.Errorf("unknown command, supported commands: set_page, update_display")
}

func (sdc *streamdeckComponent) setPage(ctx context.Context, pageName string) error {
	sdc.configLock.Lock()
	defer sdc.configLock.Unlock()

	// Validate the page exists
	keys, err := sdc.conf.GetKeysForPage(pageName)
	if err != nil {
		return err
	}

	// Clear all buttons
	err = sdc.sd.ClearAllBtns()
	if err != nil {
		return fmt.Errorf("failed to clear buttons: %w", err)
	}

	// Clear the current keys map
	sdc.keys = map[int]KeyConfig{}

	// Update to the new page
	sdc.currentPage = pageName

	// Load the new keys
	return sdc.applyKeys(ctx, keys)
}

func (sdc *streamdeckComponent) handleUpdateDisplay(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	sdc.configLock.Lock()
	defer sdc.configLock.Unlock()

	// Decode the command using mapstructure
	var updateCmd UpdateDisplayCommand
	if err := mapstructure.Decode(cmd, &updateCmd); err != nil {
		return nil, fmt.Errorf("failed to decode update_display command: %w", err)
	}

	updated := map[string]interface{}{}

	// Plan every key update before changing anything, so a bad key rejects the
	// whole command instead of leaving the keys before it already applied.
	var plans []keyPlan
	if updateCmd.Keys != nil {
		keyRefs := make([]string, 0, len(updateCmd.Keys))
		for keyRef := range updateCmd.Keys {
			keyRefs = append(keyRefs, keyRef)
		}
		sort.Strings(keyRefs)

		planned := map[int]string{}
		for _, keyRef := range keyRefs {
			p, err := sdc.planKeyUpdate(updateCmd.Page, keyRef, updateCmd.Keys[keyRef])
			if err != nil {
				return nil, err
			}
			if other, dup := planned[p.keyNum]; dup {
				return nil, fmt.Errorf("key %d is updated twice, as %q and %q", p.keyNum, other, keyRef)
			}
			planned[p.keyNum] = keyRef
			plans = append(plans, p)
		}
	}

	// Handle brightness update
	if updateCmd.Brightness != nil {
		err := sdc.updateBrightness(*updateCmd.Brightness)
		if err != nil {
			return nil, fmt.Errorf("failed to update brightness: %w", err)
		}
		sdc.conf.Brightness = *updateCmd.Brightness
		updated["brightness"] = *updateCmd.Brightness
	}

	// Handle key updates
	if updateCmd.Keys != nil {
		updatedKeys := []int{}
		for _, p := range plans {
			if err := sdc.commitKeyUpdate(ctx, p); err != nil {
				return nil, err
			}
			updatedKeys = append(updatedKeys, p.keyNum)
		}

		updated["keys"] = updatedKeys
	}

	// Handle dial updates
	if updateCmd.Dials != nil {
		updatedDials := []int{}
		for dialNumStr, dialConfigMap := range updateCmd.Dials {
			dialNum := 0
			_, err := fmt.Sscanf(dialNumStr, "%d", &dialNum)
			if err != nil {
				return nil, fmt.Errorf("invalid dial number: %s", dialNumStr)
			}

			// Find existing dial config or create new one
			existingDial := DialConfig{Dial: dialNum}
			foundDial := false
			for _, dc := range sdc.conf.Dials {
				if dc.Dial == dialNum {
					existingDial = dc
					foundDial = true
					break
				}
			}

			// Apply updates using shared helper
			newDial, err := sdc.applyDialUpdate(existingDial, dialConfigMap)
			if err != nil {
				return nil, fmt.Errorf("failed to apply updates to dial %d: %w", dialNum, err)
			}
			newDial.Dial = dialNum

			// Update dial in config
			if foundDial {
				for i := range sdc.conf.Dials {
					if sdc.conf.Dials[i].Dial == dialNum {
						sdc.conf.Dials[i] = newDial
						break
					}
				}
			} else {
				sdc.conf.Dials = append(sdc.conf.Dials, newDial)
			}

			updatedDials = append(updatedDials, dialNum)
		}

		updated["dials"] = updatedDials
	}

	if len(updated) == 0 {
		return nil, fmt.Errorf("no valid updates provided")
	}

	return updated, nil
}

// checkDrawable rejects a key that applyKeys would later fail to draw: no
// component or method, an unknown image or font, or a method other than the
// ones the module supports.
func checkDrawable(k KeyConfig) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if m := k.snakeMethod(); m != "DoCommand" && m != "SetPosition" {
		return fmt.Errorf("only support DoCommand and SetPosition now, not %s", k.Method)
	}
	// updateKey has nothing to draw for a key with no image and no text, unless
	// it is a SetPosition key, which takes its text from the switch position.
	if k.Image == "" && k.Text == "" {
		if k.snakeMethod() != "SetPosition" {
			return fmt.Errorf("nothing to display for key %d: set text or an image", k.Key)
		}
		if _, err := switchArg(k); err != nil {
			return err
		}
	}
	return nil
}

// drawKey draws one key on the deck. Tests replace it so update_display can be
// exercised without a deck attached.
func (sdc *streamdeckComponent) drawKey(ctx context.Context, k KeyConfig) error {
	if sdc.draw != nil {
		return sdc.draw(ctx, k)
	}
	return sdc.updateKey(ctx, k)
}

// locateKey resolves keyRef against keys. keyRef is either a key index ("4") or
// a key name. It returns the position of the key in keys (-1 if an index has no
// entry yet) and its key index. An unknown or ambiguous name is an error, and a
// name that looks like a number is read as a key number.
func locateKey(keys []KeyConfig, keyRef string) (pos int, keyNum int, err error) {
	if n, convErr := strconv.Atoi(keyRef); convErr == nil {
		for i := range keys {
			if keys[i].Key == n {
				return i, n, nil
			}
		}
		return -1, n, nil
	}
	pos = -1
	for i := range keys {
		if keys[i].Name != keyRef {
			continue
		}
		if pos >= 0 {
			return -1, 0, fmt.Errorf("key name %q is used by more than one key (%d and %d), address one by number", keyRef, keys[pos].Key, keys[i].Key)
		}
		pos = i
	}
	if pos < 0 {
		return -1, 0, fmt.Errorf("no key named %q", keyRef)
	}
	return pos, keys[pos].Key, nil
}

// keyPlan is a validated key update that has not been drawn or saved yet.
type keyPlan struct {
	flat   bool   // the update targets the flat keys list, not a page
	page   string // the target page, for a pages config
	pos    int    // position in the list or page, -1 if the key is new
	keyNum int
	key    KeyConfig
}

// planKeyUpdate resolves and validates one key update from update_display
// without drawing or saving anything. keyRef is a key index or a key name.
//
// With a pages config the update targets the page's keys (page, or the page
// that is showing if empty), so other pages and the page set are left intact. A
// key can only be added to the page that is showing, and an update that would
// leave a key undrawable is rejected. With a flat keys list it targets that
// list. The caller holds configLock.
func (sdc *streamdeckComponent) planKeyUpdate(page, keyRef string, updates map[string]interface{}) (keyPlan, error) {
	if len(sdc.conf.Pages) == 0 {
		// A flat keys list has no pages, so page is ignored, as it was before
		// update_display knew about pages.
		pos, keyNum, err := locateKey(sdc.conf.Keys, keyRef)
		if err != nil {
			return keyPlan{}, err
		}

		existingKey, hasExisting := sdc.keys[keyNum]
		if !hasExisting {
			existingKey = KeyConfig{Key: keyNum}
		}
		newKey, err := sdc.applyKeyUpdate(existingKey, updates)
		if err != nil {
			return keyPlan{}, fmt.Errorf("failed to apply updates to key %d: %w", keyNum, err)
		}
		newKey.Key = keyNum
		return keyPlan{flat: true, pos: pos, keyNum: keyNum, key: newKey}, nil
	}

	if page == "" {
		page = sdc.currentPage
	}
	keys, ok := sdc.conf.Pages[page]
	if !ok {
		return keyPlan{}, fmt.Errorf("page %q not found", page)
	}
	pos, keyNum, err := locateKey(keys, keyRef)
	if err != nil {
		return keyPlan{}, fmt.Errorf("page %q: %w", page, err)
	}
	if pos < 0 && page != sdc.currentPage {
		return keyPlan{}, fmt.Errorf("page %q has no key %d: a key can only be added to the page that is showing, or in the config", page, keyNum)
	}

	existingKey := KeyConfig{Key: keyNum}
	if pos >= 0 {
		existingKey = keys[pos]
	}
	newKey, err := sdc.applyKeyUpdate(existingKey, updates)
	if err != nil {
		return keyPlan{}, fmt.Errorf("failed to apply updates to key %d: %w", keyNum, err)
	}
	newKey.Key = keyNum
	// A key on a page that isn't showing isn't drawn when it is saved, so check
	// it here: a bad update left in the config would break the page, and its
	// remaining keys, the next time someone navigates to it.
	if err := checkDrawable(newKey); err != nil {
		return keyPlan{}, fmt.Errorf("page %q key %d: %w", page, keyNum, err)
	}
	return keyPlan{page: page, pos: pos, keyNum: keyNum, key: newKey}, nil
}

// commitKeyUpdate draws a planned key update, if its page is showing, and saves
// it. The caller holds configLock and has planned every update of the command
// first, so positions in the key lists are still valid.
func (sdc *streamdeckComponent) commitKeyUpdate(ctx context.Context, p keyPlan) error {
	if p.flat {
		if err := sdc.drawKey(ctx, p.key); err != nil {
			return fmt.Errorf("failed to update key %d: %w", p.keyNum, err)
		}
		sdc.keys[p.keyNum] = p.key
		if p.pos >= 0 {
			sdc.conf.Keys[p.pos] = p.key
		} else {
			sdc.conf.Keys = append(sdc.conf.Keys, p.key)
		}
		return nil
	}

	if p.page == sdc.currentPage {
		if err := sdc.drawKey(ctx, p.key); err != nil {
			return fmt.Errorf("failed to update key %d: %w", p.keyNum, err)
		}
		sdc.keys[p.keyNum] = p.key
	}
	if p.pos >= 0 {
		sdc.conf.Pages[p.page][p.pos] = p.key
	} else {
		sdc.conf.Pages[p.page] = append(sdc.conf.Pages[p.page], p.key)
	}
	return nil
}

// updateDisplayKey plans and commits one key update and returns the index of the
// key it changed.
func (sdc *streamdeckComponent) updateDisplayKey(ctx context.Context, page, keyRef string, updates map[string]interface{}) (int, error) {
	p, err := sdc.planKeyUpdate(page, keyRef, updates)
	if err != nil {
		return 0, err
	}
	if err := sdc.commitKeyUpdate(ctx, p); err != nil {
		return 0, err
	}
	return p.keyNum, nil
}
