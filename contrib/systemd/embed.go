// Package systemd embeds goboat's systemd unit so the binary can install
// the exact file that ships in this directory (`goboat setup-host`).
package systemd

import _ "embed"

// Unit is contrib/systemd/goboat.service.
//
//go:embed goboat.service
var Unit string

// UnitPath is where the unit is installed.
const UnitPath = "/etc/systemd/system/goboat.service"
