//go:build !embedcli

package cli

// Dev builds carry no embedded CLI; the runner falls back to FMSG_CLI or
// `fmsg` on PATH.
var embeddedCLI []byte
