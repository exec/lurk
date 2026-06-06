package main

import (
	"reflect"
	"testing"

	"github.com/exec/lurk/config"
)

// TestEnvBool checks the env-var boolean parser: recognized true/false spellings,
// and — crucially — that an unset or unrecognized value yields the caller's
// default rather than silently collapsing to false.
func TestEnvBool(t *testing.T) {
	const key = "LURK_TEST_BOOL"

	t.Run("unset returns the default", func(t *testing.T) {
		if envBool(key, true) != true {
			t.Errorf("unset with def=true returned false")
		}
		if envBool(key, false) != false {
			t.Errorf("unset with def=false returned true")
		}
	})

	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run("true/"+v, func(t *testing.T) {
			t.Setenv(key, v)
			if !envBool(key, false) {
				t.Errorf("envBool(%q) = false, want true", v)
			}
		})
	}
	for _, v := range []string{"0", "false", "no", "off"} {
		t.Run("false/"+v, func(t *testing.T) {
			t.Setenv(key, v)
			if envBool(key, true) {
				t.Errorf("envBool(%q) with def=true = true, want false", v)
			}
		})
	}
	for _, v := range []string{"maybe", "2", "yep"} {
		t.Run("unrecognized/"+v, func(t *testing.T) {
			t.Setenv(key, v)
			if envBool(key, true) != true {
				t.Errorf("unrecognized %q with def=true did not keep the default", v)
			}
			if envBool(key, false) != false {
				t.Errorf("unrecognized %q with def=false did not keep the default", v)
			}
		})
	}
}

// TestNetworkToConfig checks field mapping, Defaults fallback, SASL uppercasing,
// and the "lurk" nick fallback.
func TestNetworkToConfig(t *testing.T) {
	def := config.Identity{Nick: "dylan", User: "dyl", Realname: "Dylan H"}

	t.Run("explicit fields win over defaults", func(t *testing.T) {
		n := config.Network{
			Name: "Libera", Addr: "irc.libera.chat:6697", TLS: true,
			Nick: "lurkbot", User: "lb", Realname: "Lurk Bot", Pass: "srv",
			SASL:     config.SASL{Mechanism: "plain", Username: "lurkbot", Password: "pw"},
			Channels: []string{"#a", "#b"},
		}
		cfg, channels := networkToConfig(n, def)
		if cfg.Nick != "lurkbot" || cfg.User != "lb" || cfg.Realname != "Lurk Bot" {
			t.Errorf("identity = %q/%q/%q", cfg.Nick, cfg.User, cfg.Realname)
		}
		if cfg.Server != "irc.libera.chat:6697" || !cfg.TLS || cfg.Pass != "srv" {
			t.Errorf("server mapping wrong: %+v", cfg)
		}
		if cfg.SASL.Mechanism != "PLAIN" {
			t.Errorf("SASL mechanism = %q, want uppercased PLAIN", cfg.SASL.Mechanism)
		}
		if !reflect.DeepEqual(channels, []string{"#a", "#b"}) {
			t.Errorf("channels = %v", channels)
		}
	})

	t.Run("blank identity falls back to defaults", func(t *testing.T) {
		cfg, _ := networkToConfig(config.Network{Name: "X", Addr: "h:1"}, def)
		if cfg.Nick != "dylan" || cfg.User != "dyl" || cfg.Realname != "Dylan H" {
			t.Errorf("fallback identity = %q/%q/%q", cfg.Nick, cfg.User, cfg.Realname)
		}
	})

	t.Run("nick falls back to lurk when all blank", func(t *testing.T) {
		cfg, _ := networkToConfig(config.Network{Name: "X", Addr: "h:1"}, config.Identity{})
		if cfg.Nick != "lurk" {
			t.Errorf("nick = %q, want lurk", cfg.Nick)
		}
		if cfg.Realname != "lurk IRC client" {
			t.Errorf("realname = %q, want lurk IRC client", cfg.Realname)
		}
	})

	t.Run("echo-message cap requested", func(t *testing.T) {
		cfg, _ := networkToConfig(config.Network{Name: "X", Addr: "h:1"}, def)
		found := false
		for _, c := range cfg.Caps {
			if c == "echo-message" {
				found = true
			}
		}
		if !found {
			t.Errorf("Caps missing echo-message: %v", cfg.Caps)
		}
	})
}
