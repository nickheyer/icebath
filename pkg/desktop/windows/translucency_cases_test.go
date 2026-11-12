package windows

import (
	"math"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

var floorVar = float64(TranslucencyOpacityFloor)

func legacyOpacity(intensity float64) float64 { return 1 - (intensity/100)*0.7 }

func clearState(intensity int) TranslucencyState {
	return TranslucencyState{Intensity: intensity, Mode: "clear", Material: DefaultGlassMaterial, Scope: DefaultGlassScope}
}

func glassState(intensity int, material string, fade int) TranslucencyState {
	return TranslucencyState{Intensity: intensity, Fade: fade, Mode: "glass", Material: material, Scope: DefaultGlassScope}
}

func g(intensity int) TranslucencyState { return glassState(intensity, DefaultGlassMaterial, 0) }

func TestClampIntensityClampsToTheLeverBoundsAndRoundsToAWholePercent(t *testing.T) {
	for in, want := range map[any]int{-5.0: TranslucencyMin, 0.0: 0, 49.6: 50, 100.0: TranslucencyMax, 250.0: TranslucencyMax, "35": 35} {
		if got := ClampIntensity(in); got != want {
			t.Fatalf("%v -> %d", in, got)
		}
	}
}

func TestClampIntensityTreatsJunkAs0OpaqueRatherThanLettingItReachSetOpacity(t *testing.T) {
	for _, in := range []any{nil, math.NaN(), math.Inf(1), "glass"} {
		if ClampIntensity(in) != 0 {
			t.Fatal(in)
		}
	}
}

func TestNormalizeModeAcceptsGlassOnlyOnAPlatformThatHasANativeMaterial(t *testing.T) {
	if NormalizeMode("glass", true, 0) != "glass" || NormalizeMode("glass", false, 0) != "clear" {
		t.Fatal("support gate")
	}
}

func TestNormalizeModeHonoursAnExplicitChoice(t *testing.T) {
	if NormalizeMode("clear", true, 0) != "clear" || NormalizeMode("glass", true, 0) != "glass" {
		t.Fatal("explicit")
	}
}

func TestNormalizeModePreSelectsGlassWhenThePlatformSupportsItAndNothingIsRecorded(t *testing.T) {
	if NormalizeMode(nil, true, 0) != "glass" || NormalizeMode("acrylic", true, 0) != "glass" || NormalizeMode(42.0, true, 0) != "glass" || NormalizeMode(nil, false, 0) != "clear" {
		t.Fatal("preselect")
	}
}

func TestNormalizeModeLeavesAnAlreadyTunedLegacyProfileOnClear(t *testing.T) {
	if NormalizeMode(nil, true, 45) != "clear" || NormalizeMode(nil, true, 1) != "clear" || NormalizeMode(nil, true, 0) != "glass" || NormalizeMode("glass", true, 45) != "glass" {
		t.Fatal("legacy")
	}
}

func TestWindowOpacityForLeavesBothEndpointsBitIdenticalToTheLinearRampItReplaced(t *testing.T) {
	if WindowOpacityFor(clearState(TranslucencyMin)) != legacyOpacity(0) || WindowOpacityFor(clearState(TranslucencyMax)) != legacyOpacity(100) {
		t.Fatal("endpoints")
	}
}

func TestWindowOpacityForIsFullyOpaqueAt0AndExactlyTheFloorAt100(t *testing.T) {
	if WindowOpacityFor(clearState(0)) != 1 || WindowOpacityFor(clearState(100)) != 1-(1-floorVar) {
		t.Fatal("floor")
	}
}

func TestWindowOpacityForDecreasesMonotonicallyAndNeverSinksBelowTheFloor(t *testing.T) {
	prev := WindowOpacityFor(clearState(TranslucencyMin))
	for i := TranslucencyMin + 1; i <= TranslucencyMax; i++ {
		o := WindowOpacityFor(clearState(i))
		if !(o < prev) || o < TranslucencyOpacityFloor {
			t.Fatalf("%d -> %v", i, o)
		}
		prev = o
	}
}

func TestWindowOpacityForSpendsAUsefulStretchOfTheLeverOnReadableSettingsNotItsFirstFewPercent(t *testing.T) {
	readable := 0
	for i := TranslucencyMin; i <= TranslucencyMax; i++ {
		if WindowOpacityFor(clearState(i)) >= 0.95 {
			readable++
		}
	}
	if readable <= 20 {
		t.Fatal(readable)
	}
}

func TestWindowOpacityForKeepsFineControlNearTheOpaqueEnd(t *testing.T) {
	if !(WindowOpacityFor(clearState(5)) > 0.99) || !(WindowOpacityFor(clearState(1)) > legacyOpacity(1)) || !(TranslucencyCurve > 1) {
		t.Fatal("fine control")
	}
}

func TestWindowOpacityForClampsBeforeMappingSoCorruptStateCannotEscapeTheRange(t *testing.T) {
	if WindowOpacityFor(clearState(-40)) != 1 || WindowOpacityFor(clearState(240)) != WindowOpacityFor(clearState(TranslucencyMax)) {
		t.Fatal("clamp")
	}
}

func TestWindowOpacityForIgnoresTheIntensityLeverEntirelyInGlassMode(t *testing.T) {
	for _, i := range []int{0, 60, 100} {
		if WindowOpacityFor(g(i)) != 1 {
			t.Fatal(i)
		}
	}
}

func TestWindowOpacityForFadesAGlassWindowOnlyThroughItsOwnLeverOnTheRampClearUses(t *testing.T) {
	if WindowOpacityFor(glassState(60, DefaultGlassMaterial, 0)) != 1 || WindowOpacityFor(glassState(60, DefaultGlassMaterial, 40)) != WindowOpacityFor(clearState(40)) || WindowOpacityFor(glassState(100, DefaultGlassMaterial, 100)) != WindowOpacityFor(clearState(100)) {
		t.Fatal("fade ramp")
	}
}

func TestWindowOpacityForLeavesFadeInertUnderClearWhereTheIntensityLeverAlreadyIsTheOpacity(t *testing.T) {
	s := clearState(40)
	s.Fade = 100
	if WindowOpacityFor(s) != WindowOpacityFor(clearState(40)) {
		t.Fatal("fade under clear")
	}
}

func TestGlassSurfaceKeepRunsLinearToZeroSoTheTopOfTheLeverIsUntintedGlass(t *testing.T) {
	if GlassSurfaceKeep(0) != 100 || GlassSurfaceKeep(50) != 50 || GlassSurfaceKeep(100) != 0 {
		t.Fatal("keep")
	}
}

func TestGlassSurfaceKeepClampsItsInputLikeEveryOtherConsumerOfTheLever(t *testing.T) {
	if GlassSurfaceKeep(-40) != 100 || GlassSurfaceKeep(240) != 0 {
		t.Fatal("keep clamp")
	}
}

func TestNormalizeMaterialNormalizeScopeAcceptsEveryShippedMaterialAndArea(t *testing.T) {
	for _, m := range GlassMaterials {
		if NormalizeMaterial(m) != m {
			t.Fatal(m)
		}
	}
	for _, s := range GlassScopes {
		if NormalizeScope(s) != s {
			t.Fatal(s)
		}
	}
}

func TestNormalizeMaterialNormalizeScopeFallsBackForJunkAndForMaterialsDeliberatelyLeftOutOfTheLadder(t *testing.T) {
	for _, in := range []any{"sidebar", "hud", nil, 7.0} {
		if NormalizeMaterial(in) != DefaultGlassMaterial {
			t.Fatal(in)
		}
	}
	if NormalizeScope("rail") != DefaultGlassScope || NormalizeScope(nil) != DefaultGlassScope {
		t.Fatal("scope")
	}
}

func TestVibrancyForUsesTheChosenFrostMaterialOnlyWhileGlassIsActuallyOn(t *testing.T) {
	if VibrancyFor(glassState(60, "header", 0)) != "header" || VibrancyFor(glassState(60, "popover", 0)) != "popover" {
		t.Fatal("vibrancy")
	}
}

func TestVibrancyForFallsBackToTheLongStandingSidebarMaterialOtherwise(t *testing.T) {
	if VibrancyFor(glassState(0, "header", 0)) != "sidebar" || VibrancyFor(clearState(60)) != "sidebar" {
		t.Fatal("sidebar")
	}
}

func frost(v string, m string) HudFrost {
	if v == "" {
		return HudFrost{BackgroundMaterial: m}
	}
	return HudFrost{Vibrancy: &v, BackgroundMaterial: m}
}

func TestHudFrostForWearsTheChosenFrostOnBothPlatformsWhileTheBandIsShowing(t *testing.T) {
	if !reflect.DeepEqual(HudFrostFor(glassState(60, "header", 0), true), frost("header", "mica")) || !reflect.DeepEqual(HudFrostFor(glassState(60, "under-window", 0), true), frost("under-window", "acrylic")) {
		t.Fatal("showing")
	}
}

func TestHudFrostForIsOffWheneverTheBandIsNotCoveringTheWindow(t *testing.T) {
	if !reflect.DeepEqual(HudFrostFor(glassState(60, "header", 0), false), frost("", "none")) {
		t.Fatal("hidden band")
	}
}

func TestHudFrostForIsOffWheneverGlassItselfIsOff(t *testing.T) {
	if !reflect.DeepEqual(HudFrostFor(clearState(60), true), frost("", "none")) || !reflect.DeepEqual(HudFrostFor(glassState(0, "header", 0), true), frost("", "none")) {
		t.Fatal("glass off")
	}
}

func TestHudFrostForDoesNotMoveAnyNativePropertyAsTheTintSliderIsDragged(t *testing.T) {
	for i := 1; i <= 100; i++ {
		if !reflect.DeepEqual(HudFrostFor(glassState(i, "popover", 0), true), frost("popover", "tabbed")) {
			t.Fatal(i)
		}
	}
}

func TestGlassSupportedOnIsOnForMacOSRegardlessOfKernelVersion(t *testing.T) {
	if !GlassSupportedOn("darwin", "") || !GlassSupportedOn("darwin", "24.6.0") {
		t.Fatal("darwin")
	}
}

func TestGlassSupportedOnIsOnForWindows1122H2AndNewerOffForEverythingOlder(t *testing.T) {
	b := strconv.Itoa(WindowsGlassMinBuild)
	if !GlassSupportedOn("win32", "10.0."+b) || !GlassSupportedOn("win32", "10.0."+b+".1") || GlassSupportedOn("win32", "10.0."+strconv.Itoa(WindowsGlassMinBuild-1)) || GlassSupportedOn("win32", "10.0.19045") || GlassSupportedOn("win32", "10.0") || GlassSupportedOn("win32", "") {
		t.Fatal("windows builds")
	}
}

func TestGlassSupportedOnIsOffOnLinuxElectronHasNoFirstPartyDesktopMaterialThere(t *testing.T) {
	if GlassSupportedOn("linux", "6.8.0") {
		t.Fatal("linux")
	}
}

func TestBackgroundMaterialForIsNoneWhileGlassIsOffSoDWMDoesNotKeepDrawingUnderTheBacking(t *testing.T) {
	if BackgroundMaterialFor(glassState(0, "header", 0)) != "none" || BackgroundMaterialFor(clearState(60)) != "none" {
		t.Fatal("none")
	}
}

func TestBackgroundMaterialForMapsTheSheerToHeavyFrostLadderOntoAcrylicTabbedMica(t *testing.T) {
	if BackgroundMaterialFor(glassState(60, "under-window", 0)) != "acrylic" || BackgroundMaterialFor(glassState(60, "popover", 0)) != "tabbed" || BackgroundMaterialFor(glassState(60, "titlebar", 0)) != "mica" {
		t.Fatal("ladder")
	}
}

func TestBackgroundMaterialForResolvesEveryShippedRungToARealSystemMaterial(t *testing.T) {
	for _, m := range GlassMaterials {
		if !slices.Contains(WindowsBackgroundMaterials, BackgroundMaterialFor(glassState(60, m, 0))) {
			t.Fatal(m)
		}
	}
}

func TestTranslucencySupportedOnCoversTheTwoPlatformsWhereSetOpacityOrANativeMaterialExists(t *testing.T) {
	if !TranslucencySupportedOn("darwin") || !TranslucencySupportedOn("win32") {
		t.Fatal("supported")
	}
}

func TestTranslucencySupportedOnIsOffOnLinuxWhereNeitherModeDoesAnything(t *testing.T) {
	if TranslucencySupportedOn("linux") || TranslucencySupportedOn("freebsd") {
		t.Fatal("unsupported")
	}
}

func backdropsOf(materials []string) []string {
	var out []string
	for _, m := range materials {
		out = append(out, BackgroundMaterialFor(glassState(60, m, 0)))
	}
	return out
}

func asSet(v []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range v {
		m[s] = true
	}
	return m
}

func TestTheFrostRungsAPlatformOffersOffersTheWholeLadderOnMacOS(t *testing.T) {
	if !reflect.DeepEqual(GlassMaterialsFor(false), GlassMaterials) {
		t.Fatal("mac ladder")
	}
}

func TestTheFrostRungsAPlatformOffersNeverOffersTwoRungsThatRenderTheSameWindowsBackdrop(t *testing.T) {
	b := backdropsOf(GlassMaterialsFor(true))
	if len(asSet(b)) != len(b) || len(GlassMaterialsFor(true)) >= len(GlassMaterials) {
		t.Fatal(b)
	}
}

func TestTheFrostRungsAPlatformOffersKeepsEveryDistinctWindowsBackdropReachableFromThePicker(t *testing.T) {
	if !reflect.DeepEqual(asSet(backdropsOf(GlassMaterialsFor(true))), asSet(backdropsOf(GlassMaterials))) {
		t.Fatal("reachable")
	}
}

func TestTheFrostRungsAPlatformOffersFoldsADroppedRungOntoTheButtonThatLooksTheSame(t *testing.T) {
	p := GlassMaterialForPicker("header", true)
	if p != "titlebar" || !slices.Contains(GlassMaterialsFor(true), p) || BackgroundMaterialFor(glassState(60, p, 0)) != BackgroundMaterialFor(glassState(60, "header", 0)) {
		t.Fatal(p)
	}
}

func TestTheFrostRungsAPlatformOffersLeavesEveryRungAloneOnMacOSAndEveryOfferedRungAloneOnWindows(t *testing.T) {
	for _, m := range GlassMaterials {
		if GlassMaterialForPicker(m, false) != m {
			t.Fatal(m)
		}
	}
	for _, m := range GlassMaterialsFor(true) {
		if GlassMaterialForPicker(m, true) != m {
			t.Fatal(m)
		}
	}
}

func TestNormalizeStateParsesAModernPayload(t *testing.T) {
	got := NormalizeState(map[string]any{"intensity": 40.0, "fade": 15.0, "mode": "glass", "material": "header", "scope": "sidebar"}, true)
	if got != (TranslucencyState{Intensity: 40, Fade: 15, Mode: "glass", Material: "header", Scope: "sidebar"}) {
		t.Fatalf("%+v", got)
	}
}

func TestNormalizeStateKeepsALegacyIntensityOnlyPayloadOnClear(t *testing.T) {
	if got := NormalizeState(map[string]any{"intensity": 70.0}, true); got != clearState(70) {
		t.Fatalf("%+v", got)
	}
}

func TestNormalizeStateDefaultsAPayloadWrittenBeforeFadeExistedToNoFade(t *testing.T) {
	if NormalizeState(map[string]any{"intensity": 60.0, "mode": "glass"}, true).Fade != 0 {
		t.Fatal("fade")
	}
}

func TestNormalizeStateSurvivesJunkPayloads(t *testing.T) {
	base := TranslucencyState{Material: DefaultGlassMaterial, Scope: DefaultGlassScope}
	glassBase, clearBase := base, base
	glassBase.Mode, clearBase.Mode = "glass", "clear"
	if NormalizeState(nil, true) != glassBase || NormalizeState("nope", true) != glassBase {
		t.Fatal("null payload")
	}
	if NormalizeState(map[string]any{"intensity": "x", "fade": "x", "material": "nope", "mode": "glass", "scope": "nope"}, false) != clearBase {
		t.Fatal("junk payload")
	}
}

func TestGlassActiveIsOnOnlyForGlassWithNonzeroIntensity(t *testing.T) {
	if !GlassActive(g(60)) || GlassActive(g(0)) || GlassActive(clearState(60)) {
		t.Fatal("active")
	}
}

func TestAFreshProfilePreSelectsGlassWithTheFeatureStillOff(t *testing.T) {
	fresh := NormalizeState(nil, true)
	if fresh.Mode != "glass" || fresh.Intensity != TranslucencyMin || GlassActive(fresh) {
		t.Fatalf("%+v", fresh)
	}
}

func TestAFreshProfileLeavesTheWindowExactlyAsItIsToday(t *testing.T) {
	fresh := NormalizeState(nil, true)
	c, ok := WindowBackingOptions(fresh, "#101014")
	if WindowOpacityFor(fresh) != 1 || !ok || c != "#101014" {
		t.Fatal("fresh window")
	}
}

func TestWindowBackingOptionsOmitsBackgroundColorEntirelyWhileGlassIsActive(t *testing.T) {
	if _, ok := WindowBackingOptions(g(60), "#111111"); ok {
		t.Fatal("backing while glass")
	}
}

func TestWindowBackingOptionsKeepsTheThemedAntiFlashBackingInEveryOtherState(t *testing.T) {
	for _, c := range []struct {
		s     TranslucencyState
		color string
	}{{g(0), "#111111"}, {clearState(60), "#111111"}, {clearState(0), "#f7f7f7"}} {
		if got, ok := WindowBackingOptions(c.s, c.color); !ok || got != c.color {
			t.Fatal(c)
		}
	}
}

func TestWindowBackgroundMaterialOptionsOmitsTheWindowsBackdropWhenGlassIsInactive(t *testing.T) {
	for _, c := range []struct {
		s          TranslucencyState
		win, glass bool
	}{{g(0), true, true}, {clearState(60), true, true}, {g(60), false, true}, {g(60), true, false}} {
		if _, ok := WindowBackgroundMaterialOptions(c.s, c.win, c.glass); ok {
			t.Fatal(c)
		}
	}
}

func TestWindowBackgroundMaterialOptionsPassesTheSelectedBackdropOnlyForActiveSupportedWindowsGlass(t *testing.T) {
	if m, ok := WindowBackgroundMaterialOptions(glassState(60, "under-window", 0), true, true); !ok || m != "acrylic" {
		t.Fatal(m)
	}
}

func TestWindowOpacityOptionsOmitsOpacityEntirelyWhenTheWindowIsFullyOpaque(t *testing.T) {
	if _, ok := WindowOpacityOptions(g(60), true); ok {
		t.Fatal("glass")
	}
	if _, ok := WindowOpacityOptions(clearState(0), true); ok {
		t.Fatal("clear 0")
	}
}

func TestWindowOpacityOptionsPassesItTheMomentTheStateActuallyFades(t *testing.T) {
	for _, s := range []TranslucencyState{clearState(1), clearState(100), glassState(60, DefaultGlassMaterial, 1)} {
		if o, ok := WindowOpacityOptions(s, true); !ok || o != WindowOpacityFor(s) {
			t.Fatal(s)
		}
	}
}

func TestWindowOpacityOptionsLeavesEveryUntouchedWindowsWindowUnlayered(t *testing.T) {
	for _, a := range []string{"light", "dark"} {
		s := DefaultTranslucencyValues(a, true)
		s.Mode = "glass"
		if _, ok := WindowOpacityOptions(s, true); ok {
			t.Fatal(a)
		}
	}
}

func TestNativeOpacityForLeavesAPlatformWithoutTranslucencyFullyOpaque(t *testing.T) {
	if NativeOpacityFor(clearState(29), false) != 1 || NativeOpacityFor(clearState(100), false) != 1 || NativeOpacityFor(glassState(60, DefaultGlassMaterial, 40), false) != 1 {
		t.Fatal("unsupported")
	}
}

func TestNativeOpacityForAnswersTheRampUnchangedWhereTranslucencyIsSupported(t *testing.T) {
	for _, s := range []TranslucencyState{clearState(0), clearState(29), clearState(100)} {
		if NativeOpacityFor(s, true) != WindowOpacityFor(s) {
			t.Fatal(s)
		}
	}
}

func TestNativeOpacityForHasSomethingToSayToAWindowThatIsAlreadyFaded(t *testing.T) {
	if !OpacityNeedsSetting(NativeOpacityFor(clearState(29), false), 0.9372) {
		t.Fatal("faded")
	}
}

func TestNativeOpacityForKeepsTheConstructorOptionOffAnUnsupportedPlatform(t *testing.T) {
	if _, ok := WindowOpacityOptions(clearState(29), false); ok {
		t.Fatal("29")
	}
	if _, ok := WindowOpacityOptions(clearState(100), false); ok {
		t.Fatal("100")
	}
}

func TestOpacityNeedsSettingSkipsTheCallForAnOpaqueWindowThatHasNeverBeenFaded(t *testing.T) {
	if OpacityNeedsSetting(1, 1) {
		t.Fatal("opaque")
	}
}

func TestOpacityNeedsSettingMakesTheCallWheneverTheTargetFades(t *testing.T) {
	if !OpacityNeedsSetting(0.5, 1) || !OpacityNeedsSetting(0.9999, 1) {
		t.Fatal("fades")
	}
}

func TestOpacityNeedsSettingMakesTheCallToReturnAnAlreadyFadedWindowToOpaque(t *testing.T) {
	if !OpacityNeedsSetting(1, 0.5) {
		t.Fatal("restore")
	}
}

func nativeDiff(p, n TranslucencyState) TranslucencyChange {
	return TranslucencyChange{Backing: GlassActive(p) != GlassActive(n), Material: VibrancyFor(p) != VibrancyFor(n), Opacity: WindowOpacityFor(p) != WindowOpacityFor(n)}
}

func TestWhatAnUpdateActuallyChangesNativelyIsNothingAtAllWhileDraggingIntensityUnderGlass(t *testing.T) {
	for i := 41; i <= 100; i++ {
		if nativeDiff(g(i-1), g(i)) != (TranslucencyChange{}) {
			t.Fatal(i)
		}
	}
}

func TestWhatAnUpdateActuallyChangesNativelyIsOnlyTheOpacityWhileDraggingIntensityUnderClear(t *testing.T) {
	if nativeDiff(clearState(40), clearState(41)) != (TranslucencyChange{Opacity: true}) {
		t.Fatal("clear drag")
	}
}

func TestWhatAnUpdateActuallyChangesNativelyIsOnlyTheOpacityWhileDraggingFadeUnderGlass(t *testing.T) {
	if nativeDiff(glassState(60, DefaultGlassMaterial, 40), glassState(60, DefaultGlassMaterial, 41)) != (TranslucencyChange{Opacity: true}) {
		t.Fatal("fade drag")
	}
}

func TestWhatAnUpdateActuallyChangesNativelyIsTheMaterialAloneWhenTheFrostLevelChanges(t *testing.T) {
	if nativeDiff(glassState(60, "under-window", 0), glassState(60, "header", 0)) != (TranslucencyChange{Material: true}) || BackgroundMaterialFor(glassState(60, "under-window", 0)) == BackgroundMaterialFor(glassState(60, "header", 0)) {
		t.Fatal("frost change")
	}
}

func TestWhatAnUpdateActuallyChangesNativelyIsTheBackingAndMaterialWhenGlassCrossesZero(t *testing.T) {
	want := TranslucencyChange{Backing: true, Material: true}
	if nativeDiff(g(0), g(1)) != want || nativeDiff(g(1), g(0)) != want {
		t.Fatal("zero crossing")
	}
}

func TestWhatAnUpdateActuallyChangesNativelyIsEverythingWhenSwitchingBetweenTheTwoModes(t *testing.T) {
	if nativeDiff(clearState(60), g(60)) != (TranslucencyChange{true, true, true}) {
		t.Fatal("mode switch")
	}
}

func TestWhatAnUpdateActuallyChangesNativelyLeavesAWindowAloneWhenGlassIsSelectedButOff(t *testing.T) {
	if WindowOpacityFor(glassState(0, DefaultGlassMaterial, 1)) != 1 || WindowOpacityFor(glassState(0, DefaultGlassMaterial, 40)) != 1 {
		t.Fatal("selected but off")
	}
}

func TestWhatAnUpdateActuallyChangesNativelyStillFadesAWindowWhoseGlassIsActuallyOn(t *testing.T) {
	if !(WindowOpacityFor(glassState(66, DefaultGlassMaterial, 40)) < 1) {
		t.Fatal("glass fade")
	}
}

func allDefaults() []TranslucencyState {
	return []TranslucencyState{DefaultTranslucencyValues("light", false), DefaultTranslucencyValues("dark", false), DefaultTranslucencyValues("light", true), DefaultTranslucencyValues("dark", true)}
}

func TestTheDefaultsAFreshProfileLandsOnShipsGlassOnNotALeverRestingAtZero(t *testing.T) {
	for _, v := range allDefaults() {
		v.Mode = "glass"
		if v.Intensity <= 0 || !GlassActive(v) {
			t.Fatalf("%+v", v)
		}
	}
	for _, a := range []string{"light", "dark"} {
		if DefaultTranslucencyState(a, true, false).Mode != "glass" || DefaultTranslucencyState(a, true, true).Mode != "glass" {
			t.Fatal(a)
		}
	}
}

func TestTheDefaultsAFreshProfileLandsOnFallsBackToClearWhereNoNativeMaterialExists(t *testing.T) {
	if DefaultTranslucencyState("dark", false, false).Mode != "clear" {
		t.Fatal("clear fallback")
	}
}

func TestTheDefaultsAFreshProfileLandsOnKeepsTheContentColumnOpaqueAtTheNativeLevel(t *testing.T) {
	for _, v := range allDefaults() {
		v.Mode = "glass"
		if WindowOpacityFor(v) != 1 {
			t.Fatalf("%+v", v)
		}
	}
}

func TestTheDefaultsAFreshProfileLandsOnDefaultsEachPlatformOntoAFrostThatPlatformCanActuallyRender(t *testing.T) {
	for _, a := range []string{"light", "dark"} {
		if !slices.Contains(GlassMaterialsFor(true), DefaultTranslucencyValues(a, true).Material) || !slices.Contains(GlassMaterialsFor(false), DefaultTranslucencyValues(a, false).Material) {
			t.Fatal(a)
		}
	}
}

func TestTheDefaultsAFreshProfileLandsOnUsesTheNormalizedScopeDefaultForEveryAppearanceAndPlatform(t *testing.T) {
	for _, v := range allDefaults() {
		if v.Scope != NormalizeScope(nil) {
			t.Fatal(v)
		}
	}
}

func TestResolvingTheBookAgreesWithTheNativeFirstWindowDefaultsInEitherAppearance(t *testing.T) {
	empty := NormalizeBook(nil, true)
	for _, a := range []string{"light", "dark"} {
		for _, w := range []bool{false, true} {
			if ResolveTranslucency(empty, a, w) != DefaultTranslucencyState(a, true, w) {
				t.Fatal(a, w)
			}
		}
	}
}

func TestResolvingTheBookScopesAnEditToTheAppearanceItWasMadeIn(t *testing.T) {
	book := SetTranslucencyValues(NormalizeBook(nil, true), "light", map[string]any{"intensity": 90.0})
	if ResolveTranslucency(book, "light", false).Intensity != 90 || ResolveTranslucency(book, "dark", false).Intensity != DefaultTranslucencyValues("dark", false).Intensity {
		t.Fatal("scoped edit")
	}
}

func TestResolvingTheBookPreservesASavedWholeWindowTreatmentInBothAppearances(t *testing.T) {
	migrated := NormalizeBook(map[string]any{"intensity": 40.0, "scope": "window", "mode": "glass"}, true)
	forty, window := 40, "window"
	if !reflect.DeepEqual(migrated.Base, TranslucencyValues{Intensity: &forty, Scope: &window}) {
		t.Fatalf("%+v", migrated.Base)
	}
	for _, a := range []string{"light", "dark"} {
		for _, w := range []bool{false, true} {
			r := ResolveTranslucency(migrated, a, w)
			if r.Intensity != 40 || r.Scope != "window" || r.Mode != "glass" {
				t.Fatalf("%+v", r)
			}
		}
	}
}

func TestResolvingTheBookLetsAnAppearanceOverrideBaseWithoutDisturbingTheOther(t *testing.T) {
	tuned := SetTranslucencyValues(NormalizeBook(map[string]any{"intensity": 40.0, "mode": "glass"}, true), "dark", map[string]any{"intensity": 10.0})
	if ResolveTranslucency(tuned, "dark", false).Intensity != 10 || ResolveTranslucency(tuned, "light", false).Intensity != 40 {
		t.Fatal("override")
	}
}

func TestResolvingTheBookInheritsPerKEYNotPerAppearance(t *testing.T) {
	book := SetTranslucencyValues(NormalizeBook(map[string]any{"material": "popover", "mode": "glass"}, true), "dark", map[string]any{"intensity": 33.0})
	r := ResolveTranslucency(book, "dark", false)
	if r.Intensity != 33 || r.Material != "popover" {
		t.Fatalf("%+v", r)
	}
}

func TestResolvingTheBookKeepsModeGlobalClearVsGlassIsAboutTheWindowNotThePalette(t *testing.T) {
	empty := NormalizeBook(nil, true)
	empty.Mode = "clear"
	book := SetTranslucencyValues(empty, "light", map[string]any{"intensity": 50.0})
	if ResolveTranslucency(book, "light", false).Mode != "clear" || ResolveTranslucency(book, "dark", false).Mode != "clear" {
		t.Fatal("mode global")
	}
}
