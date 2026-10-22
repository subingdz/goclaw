// Catalog parity for the community skill registry keys. Same reasoning as
// git_keys_parity_test.go: a key added to keys.go without translations shows the raw
// "error.skill_registry_*" string to vi/zh/ru operators instead of a message.
package i18n

import (
	"strings"
	"testing"
)

var registryKeys = []string{
	MsgSkillRegistryDisabled,
	MsgSkillRegistrySourceUnknown,
	MsgSkillRegistryRefused,
}

func TestI18nCatalogs_HasRegistryKeys(t *testing.T) {
	for _, locale := range []string{LocaleEN, LocaleVI, LocaleZH, LocaleRU} {
		for _, key := range registryKeys {
			msg := lookup(locale, key)
			if msg == key {
				t.Errorf("locale=%s key=%s falls back to key string (translation missing)", locale, key)
				continue
			}
			if strings.TrimSpace(msg) == "" {
				t.Errorf("locale=%s key=%s has empty translation", locale, key)
			}
		}
	}
}
