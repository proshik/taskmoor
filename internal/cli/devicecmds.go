package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/text"
)

// deviceCmd pairs, lists and revokes the devices that reach the board from
// outside (stage 9).
func (r runner) deviceCmd(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return r.usageErr(usagef("device takes pair, list or revoke ID"))
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	w := r.env.Stdout
	switch args[0] {
	case "pair":
		if len(args) != 1 {
			return r.usageErr(usagef("device pair takes no arguments"))
		}
		code, url, err := c.DevicePair(ctx)
		if err != nil {
			return r.serviceFail(err, false)
		}
		fmt.Fprintf(w, "on the device, open %s, sign in, and enter the code %s within 10 minutes;\nit then registers a passkey, which confirms each Accept, Approve and allowed request from it\n", url, code)
	case "list":
		fs := flag.NewFlagSet("device list", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		if _, err := parse(fs, args[1:]); err != nil {
			return r.fail(err, *asJSON)
		}
		ds, err := c.Devices(ctx)
		if err != nil {
			return r.serviceFail(err, *asJSON)
		}
		if *asJSON {
			if ds == nil {
				ds = []serve.DeviceView{}
			}
			_ = writeJSON(w, map[string]any{"format": 1, "devices": ds})
			return ExitOK
		}
		if len(ds) == 0 {
			fmt.Fprintln(w, "no devices: taskmoor device pair")
		}
		for _, d := range ds {
			seen := d.LastSeen
			if seen == "" {
				seen = "never"
			}
			fmt.Fprintf(w, "%s  %-20s paired %s, last seen %s\n", d.ID, text.Line(d.Name), d.Created, seen)
		}
	case "revoke":
		if len(args) != 2 {
			return r.usageErr(usagef("device revoke takes one device id"))
		}
		if err := c.RevokeDevice(ctx, args[1]); err != nil {
			return r.serviceFail(err, false)
		}
		fmt.Fprintf(w, "%s revoked: its token and passkey no longer work\n", args[1])
	default:
		return r.usageErr(usagef("device takes pair, list or revoke ID"))
	}
	return ExitOK
}

// remoteCmd writes the files of the web from outside: the Mac's tunnel
// agent and the VPS's Caddy, Pocket ID and oauth2-proxy. It installs and
// deploys nothing.
func (r runner) remoteCmd(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "files" {
		return r.usageErr(usagef("remote takes files --vps USER@HOST --auth-domain DOMAIN [--vps-port N] [--key PATH] [--out DIR]"))
	}
	fs := flag.NewFlagSet("remote files", flag.ContinueOnError)
	vps := fs.String("vps", "", "")
	auth := fs.String("auth-domain", "", "")
	vpsPort := fs.Int("vps-port", 0, "")
	key := fs.String("key", "", "")
	out := fs.String("out", "", "")
	force := fs.Bool("force", false, "")
	pos, err := parse(fs, args[1:])
	if err == nil && (len(pos) > 0 || *vps == "" || *auth == "") {
		err = usagef("remote files takes --vps USER@HOST and --auth-domain DOMAIN")
	}
	if err != nil {
		return r.usageErr(err)
	}
	p, err := r.paths()
	if err != nil {
		return r.fail(err, false)
	}
	cfg, err := serve.LoadConfig(p.Config)
	if err != nil {
		return r.fail(err, false)
	}
	if cfg.RemoteHost == "" {
		return r.fail(fmt.Errorf("set remote_host (the board's domain) and remote_port (a free port on the Mac) in %s first", p.Config), false)
	}
	home, _ := os.UserHomeDir()
	if *key == "" {
		*key = filepath.Join(home, ".ssh", "taskmoor_tunnel")
	}
	if *vpsPort == 0 {
		*vpsPort = cfg.RemotePort
	}
	if *out == "" {
		*out = filepath.Join(p.Store, "remote")
	}
	files, err := serve.RemoteFiles(serve.RemoteFilesRequest{Domain: cfg.RemoteHost, AuthDomain: *auth, VPS: *vps,
		VPSPort: *vpsPort, MacPort: cfg.RemotePort, Key: *key, Store: p.Store})
	if err != nil {
		return r.serviceFail(err, false)
	}
	if !*force {
		if _, err := os.Stat(filepath.Join(*out, "vps", ".env")); err == nil {
			return r.fail(fmt.Errorf("%s already holds the files (and their secrets): --force writes new ones", *out), false)
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// the secrets last: a run that fails before them leaves nothing that
	// blocks the next one
	sort.Slice(names, func(i, j int) bool {
		ei, ej := strings.HasSuffix(names[i], ".env"), strings.HasSuffix(names[j], ".env")
		if ei != ej {
			return ej
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		path := filepath.Join(*out, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return r.fail(err, false)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".env") {
			mode = 0o600 // its secrets
		}
		if err := writeNew(path, []byte(files[name]), mode); err != nil {
			return r.fail(err, false)
		}
	}
	fmt.Fprintf(r.env.Stdout, "wrote %s (vps/.env holds new secrets, mode 0600); follow %s\nnothing was installed or deployed\n", *out, filepath.Join(*out, "README.md"))
	return ExitOK
}

// writeNew writes a file through a new temporary one of the given mode and
// renames it into place: a secret is never in a file of another mode.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
