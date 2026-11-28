package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func writeSupply(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadPower(t *testing.T) {
	t.Run("desktop without battery", func(t *testing.T) {
		dir := t.TempDir()
		onBat, p := readPower(dir)
		if onBat || p != nil {
			t.Fatalf("got (%v, %+v), want (false, nil)", onBat, p)
		}
	})

	t.Run("underpowered USB-C draining", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "macsmc-ac", map[string]string{
			"type":              "Mains",
			"online":            "1",
			"input_power_limit": "7500000",
		})
		writeSupply(t, dir, "macsmc-battery", map[string]string{
			"type":      "Battery",
			"capacity":  "5",
			"power_now": "-2122000",
			"status":    "Charging",
		})
		onBat, p := readPower(dir)
		if onBat {
			t.Fatalf("expected mains online (not unplugged), got onBattery=true")
		}
		if p == nil || !p.Draining {
			t.Fatalf("expected Draining=true, got %+v", p)
		}
		if p.BatteryPercent == nil || *p.BatteryPercent != 5 {
			t.Fatalf("battery percent = %v, want 5", p.BatteryPercent)
		}
		if p.BatteryPowerUW == nil || *p.BatteryPowerUW != -2122000 {
			t.Fatalf("battery power = %v, want -2122000", p.BatteryPowerUW)
		}
		if p.InputPowerLimitUW != 7500000 {
			t.Fatalf("input power limit = %d, want 7500000", p.InputPowerLimitUW)
		}
		bad, detail := (api.Health{OnBattery: onBat, Power: p}).PowerTrouble()
		if !bad || detail == "" {
			t.Fatalf("expected PowerTrouble for draining underpowered AC, got (%v, %q)", bad, detail)
		}
	})

	t.Run("15W USB-C idle full is not draining", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "macsmc-ac", map[string]string{
			"type":              "Mains",
			"online":            "1",
			"input_power_limit": "15000000",
		})
		writeSupply(t, dir, "macsmc-battery", map[string]string{
			"type":      "Battery",
			"capacity":  "100",
			"power_now": "0",
			"status":    "Full",
		})
		onBat, p := readPower(dir)
		if onBat || p == nil || p.Draining {
			t.Fatalf("got (%v, %+v), want not draining", onBat, p)
		}
		if bad, why := (api.Health{OnBattery: onBat, Power: p}).PowerTrouble(); bad {
			t.Fatalf("unexpected trouble: %s", why)
		}
	})

	t.Run("full charger momentary dip is not underpowered draining", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "macsmc-ac", map[string]string{
			"type":              "Mains",
			"online":            "1",
			"input_power_limit": "139720000",
		})
		writeSupply(t, dir, "macsmc-battery", map[string]string{
			"type":      "Battery",
			"capacity":  "88",
			"power_now": "-500000",
		})
		onBat, p := readPower(dir)
		if onBat || p == nil || p.Draining {
			t.Fatalf("got (%v, %+v), want Draining=false on 140W charger", onBat, p)
		}
		if bad, why := (api.Health{OnBattery: onBat, Power: p}).PowerTrouble(); bad {
			t.Fatalf("unexpected trouble on 140W charger at 88%%: %s", why)
		}
	})

	t.Run("unplugged battery with voltage and current fallback", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "macsmc-ac", map[string]string{
			"type":   "Mains",
			"online": "0",
		})
		writeSupply(t, dir, "macsmc-battery", map[string]string{
			"type":        "Battery",
			"capacity":    "42",
			"voltage_now": "11500000",
			"current_now": "-1000000",
		})
		onBat, p := readPower(dir)
		if !onBat || p == nil || p.Draining {
			t.Fatalf("got (%v, %+v), want onBattery=true, Draining=false", onBat, p)
		}
		if p.BatteryPowerUW == nil || *p.BatteryPowerUW != -11500000 {
			t.Fatalf("fallback power = %v, want -11500000", p.BatteryPowerUW)
		}
	})

	t.Run("peripheral mouse battery ignored in favor of macsmc-battery", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "macsmc-ac", map[string]string{
			"type":              "Mains",
			"online":            "1",
			"input_power_limit": "139720000",
		})
		// hidpp_battery_0 sorts alphabetically before macsmc-battery.
		writeSupply(t, dir, "hidpp_battery_0", map[string]string{
			"type":     "Battery",
			"scope":    "Device",
			"capacity": "5",
		})
		writeSupply(t, dir, "macsmc-battery", map[string]string{
			"type":      "Battery",
			"scope":     "System",
			"capacity":  "85",
			"power_now": "5000000",
		})
		onBat, p := readPower(dir)
		if onBat || p == nil {
			t.Fatalf("got (%v, %+v), want system battery telemetry", onBat, p)
		}
		if p.BatteryPercent == nil || *p.BatteryPercent != 85 {
			t.Fatalf("battery percent = %v, want 85 (not mouse 5%%)", p.BatteryPercent)
		}
		if p.BatteryPowerUW == nil || *p.BatteryPowerUW != 5000000 {
			t.Fatalf("battery power = %v, want 5000000", p.BatteryPowerUW)
		}
		if bad, why := (api.Health{OnBattery: onBat, Power: p}).PowerTrouble(); bad {
			t.Fatalf("unexpected trouble from low mouse battery: %s", why)
		}
	})

	t.Run("desktop with only peripheral device battery returns nil power", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "hidpp_battery_0", map[string]string{
			"type":     "Battery",
			"scope":    "Device",
			"capacity": "5",
		})
		onBat, p := readPower(dir)
		if onBat || p != nil {
			t.Fatalf("got (%v, %+v), want (false, nil) for desktop with mouse battery", onBat, p)
		}
	})

	t.Run("non-SMC system battery preferred over device battery", func(t *testing.T) {
		dir := t.TempDir()
		writeSupply(t, dir, "aaa-mouse", map[string]string{
			"type":     "Battery",
			"scope":    "Device",
			"capacity": "10",
		})
		writeSupply(t, dir, "bat-no-scope", map[string]string{
			"type":     "Battery",
			"capacity": "60",
		})
		writeSupply(t, dir, "bat-system", map[string]string{
			"type":     "Battery",
			"scope":    "System",
			"capacity": "72",
		})
		_, p := readPower(dir)
		if p == nil || p.BatteryPercent == nil || *p.BatteryPercent != 72 {
			t.Fatalf("got %+v, want scope=System battery at 72%%", p)
		}
	})
}
