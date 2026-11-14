package connections

import (
	"encoding/json"
	"os"
	"os/user"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRegistersTheRendererChannelAndReportsNativeMachineFactsWithTheOSLocale(t *testing.T) {
	saved := appLocale
	appLocale = func() string { return "fr-CA" }
	t.Cleanup(func() { appLocale = saved })

	result := (&Accounts{}).GetMachineProfile()

	var ageDays *int
	home, _ := os.UserHomeDir()
	if born, ok := fileBirthtime(home); ok && born.UnixNano() > 0 {
		days := int(max(0, (time.Now().UnixMilli()*int64(time.Millisecond)-born.UnixNano())/int64(24*time.Hour)))
		ageDays = &days
	}
	u, _ := user.Current()
	username := u.Username
	if runtime.GOOS == "windows" {
		username = username[strings.LastIndex(username, `\`)+1:]
	}
	want := MachineProfile{AgeDays: ageDays, Arch: machineArch(runtime.GOARCH), Locale: "fr-CA", Platform: machinePlatform(runtime.GOOS), Release: osRelease(), Username: username}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("got %+v want %+v", result, want)
	}
	encoded, _ := json.Marshal(result)
	var shape map[string]any
	json.Unmarshal(encoded, &shape)
	for _, key := range []string{"ageDays", "arch", "locale", "platform", "release", "username"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("missing %s in %s", key, encoded)
		}
	}
}

func TestApplicationLocaleResolvesAgainstTheShippedChromiumLocales(t *testing.T) {
	cases := map[string][]string{
		"fr":     {"fr_CA.UTF-8"},
		"es-419": {"es_MX.UTF-8"},
		"es":     {"es_ES"},
		"pt-PT":  {"pt_AO"},
		"pt-BR":  {"pt"},
		"zh-TW":  {"zh_HK.UTF-8"},
		"zh-CN":  {"zh_SG"},
		"en-GB":  {"en_AU.UTF-8"},
		"en-US":  {"C"},
		"he":     {"iw_IL"},
		"nb":     {"no_NO"},
		"de":     {"xx_YY", "de_AT@euro"},
	}
	for want, preferred := range cases {
		if got := ApplicationLocale(preferred); got != want {
			t.Fatalf("%v: got %s want %s", preferred, got, want)
		}
	}
	if got := ApplicationLocale(envPreferredLanguages(envOf(map[string]string{"LANGUAGE": "ja:en", "LANG": "de_DE.UTF-8"}))); got != "ja" {
		t.Fatalf("LANGUAGE list: got %s", got)
	}
	if got := ApplicationLocale(envPreferredLanguages(envOf(map[string]string{"LC_MESSAGES": "ko_KR.UTF-8", "LANG": "de_DE.UTF-8"}))); got != "ko" {
		t.Fatalf("LC_MESSAGES: got %s", got)
	}
	if got := parseAppleLanguages("(\n    \"en-GB\",\n    \"fr-CA\"\n)\n"); !reflect.DeepEqual(got, []string{"en-GB", "fr-CA"}) {
		t.Fatalf("AppleLanguages: got %v", got)
	}
}
