package serve

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// The files of the web from outside (stage 9, C5, A10): the Mac's launchd
// agent that keeps the reverse tunnel up, and the VPS's Caddy, Pocket ID
// (passkey-only sign-in) and oauth2-proxy (Caddy's forward_auth). taskmoor
// writes them; the person applies them (nothing is installed or deployed
// by taskmoor).

// The images, pinned (each at least two weeks old when chosen).
const (
	caddyImage       = "caddy:2.11.8"
	pocketIDImage    = "ghcr.io/pocket-id/pocket-id:v2.19.1"
	oauth2ProxyImage = "quay.io/oauth2-proxy/oauth2-proxy:v7.15.5"
)

// The VPS's own loopback ports, which the tunnel may not take.
const (
	pocketIDPort    = 1411
	oauth2ProxyPort = 4180
)

// RemoteFilesRequest names what the files need.
type RemoteFilesRequest struct {
	Domain     string // remote_host: the board's domain
	AuthDomain string // Pocket ID's domain
	VPS        string // user@host the tunnel logs in as
	VPSPort    int    // the tunnel's port on the VPS's loopback
	MacPort    int    // remote_port
	Key        string // the tunnel's private key on the Mac
	Store      string // for the agent's log
}

var (
	domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	vpsRe    = regexp.MustCompile(`^[a-z_][a-z0-9_-]*@[A-Za-z0-9.-]+$`)
)

// RemoteFiles renders the files, by path under the output directory; the
// secrets (Pocket ID's encryption key, oauth2-proxy's cookie secret) are
// made here, once.
func RemoteFiles(req RemoteFilesRequest) (map[string]string, error) {
	switch {
	case !domainRe.MatchString(req.Domain):
		return nil, refusedf("remote_host %q is not a domain name", req.Domain)
	case !domainRe.MatchString(req.AuthDomain) || req.AuthDomain == req.Domain:
		return nil, refusedf("--auth-domain %q must be a domain name of its own (Pocket ID's)", req.AuthDomain)
	case !vpsRe.MatchString(req.VPS):
		return nil, refusedf("--vps %q is not user@host", req.VPS)
	case req.VPSPort < 1024 || req.VPSPort > 65535:
		return nil, refusedf("--vps-port %d is not an unprivileged port", req.VPSPort)
	case req.VPSPort == pocketIDPort:
		return nil, refusedf("--vps-port %d is Pocket ID's on the VPS: choose another (it defaults to remote_port)", req.VPSPort)
	case req.VPSPort == oauth2ProxyPort:
		return nil, refusedf("--vps-port %d is oauth2-proxy's on the VPS: choose another (it defaults to remote_port)", req.VPSPort)
	case req.MacPort < 1 || req.MacPort > 65535:
		return nil, refusedf("remote_port is not set")
	}
	secret := func() string {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return base64.URLEncoding.EncodeToString(b)
	}
	user := req.VPS[:strings.Index(req.VPS, "@")]
	files := map[string]string{}

	files["mac/com.taskmoor.tunnel.plist"] = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.taskmoor.tunnel</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/ssh</string>
    <string>-N</string>
    <string>-o</string><string>BatchMode=yes</string>
    <string>-o</string><string>ExitOnForwardFailure=yes</string>
    <string>-o</string><string>ServerAliveInterval=30</string>
    <string>-o</string><string>ServerAliveCountMax=3</string>
    <string>-o</string><string>IdentitiesOnly=yes</string>
    <string>-o</string><string>StrictHostKeyChecking=yes</string>
    <string>-i</string><string>%s</string>
    <string>-R</string><string>127.0.0.1:%d:127.0.0.1:%d</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>15</integer>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, xmlEscape(req.Key), req.VPSPort, req.MacPort, xmlEscape(req.VPS), xmlEscape(filepath.Join(req.Store, "tunnel.log")))

	files["vps/authorized_keys.line"] = fmt.Sprintf(
		"restrict,port-forwarding,permitlisten=\"127.0.0.1:%d\",permitopen=\"127.0.0.1:1\",command=\"/usr/sbin/nologin\" PASTE-THE-PUBLIC-KEY-HERE taskmoor-tunnel\n", req.VPSPort)
	// the account's own sshd policy: the key option alone leaves local
	// forwarding (-L) to whatever the VPS reaches, and GatewayPorts could
	// make the listener public
	files["vps/sshd-taskmoor-tunnel.conf"] = fmt.Sprintf(`# taskmoor's tunnel account: it may only listen on 127.0.0.1:%[2]d.
# Install as /etc/ssh/sshd_config.d/taskmoor-tunnel.conf (or append to
# sshd_config), check with: sudo sshd -t, then reload sshd.
Match User %[1]s
	AllowTcpForwarding remote
	PermitListen 127.0.0.1:%[2]d
	PermitOpen none
	GatewayPorts no
	AllowStreamLocalForwarding no
	AllowAgentForwarding no
	X11Forwarding no
	PermitTTY no
	ForceCommand /usr/sbin/nologin
	# a lost connection lets go of the listener, so the next one can take it
	ClientAliveInterval 30
	ClientAliveCountMax 3
`, user, req.VPSPort)

	files["vps/.env"] = fmt.Sprintf(`# taskmoor's web from outside — secrets: keep this file 0600 on the VPS.
DOMAIN=%s
AUTH_DOMAIN=%s
TUNNEL_PORT=%d

# Who may reach Pocket ID. Until you have created your account (its first
# account is its administrator), only your own address: put it here (e.g.
# 203.0.113.7/32), then, after the setup, 0.0.0.0/0 ::/0 and recreate the
# containers (README: docker compose up -d --force-recreate; a restart keeps
# the old value).
#
# These secrets belong to one deployment: Pocket ID's data is encrypted with
# ENCRYPTION_KEY. Keep this file with the VPS's data; new files made with
# --force are for a new deployment only.
AUTH_OPEN_TO=127.0.0.1/32

# Pocket ID (passkey-only sign-in)
APP_URL=https://%s
TRUST_PROXY=true
ENCRYPTION_KEY=%s

# oauth2-proxy (Caddy's forward_auth): fill in the client from Pocket ID
OAUTH2_PROXY_PROVIDER=oidc
OAUTH2_PROXY_OIDC_ISSUER_URL=https://%s
OAUTH2_PROXY_CLIENT_ID=
OAUTH2_PROXY_CLIENT_SECRET=
OAUTH2_PROXY_REDIRECT_URL=https://%s/oauth2/callback
OAUTH2_PROXY_CODE_CHALLENGE_METHOD=S256
OAUTH2_PROXY_SCOPE=openid email profile
OAUTH2_PROXY_EMAIL_DOMAINS=*
OAUTH2_PROXY_COOKIE_SECRET=%s
OAUTH2_PROXY_COOKIE_SECURE=true
OAUTH2_PROXY_COOKIE_DOMAINS=%s
OAUTH2_PROXY_WHITELIST_DOMAINS=%s
OAUTH2_PROXY_REVERSE_PROXY=true
OAUTH2_PROXY_SKIP_PROVIDER_BUTTON=true
OAUTH2_PROXY_HTTP_ADDRESS=127.0.0.1:4180
OAUTH2_PROXY_UPSTREAMS=static://202
# Pocket ID leaves its first account's email unverified; nothing here
# decides by email (one account, the client limited to it), so that is
# accepted
OAUTH2_PROXY_INSECURE_OIDC_ALLOW_UNVERIFIED_EMAIL=true
`, req.Domain, req.AuthDomain, req.VPSPort, req.AuthDomain, secret(), req.AuthDomain, req.Domain, secret(), req.Domain, req.Domain)

	files["vps/docker-compose.yml"] = fmt.Sprintf(`# taskmoor's web from outside on the VPS: Caddy (HTTPS) in front, Pocket ID
# for the passkey sign-in, oauth2-proxy as Caddy's forward_auth. Caddy and
# oauth2-proxy use the host's network and listen on 127.0.0.1 (oauth2-proxy)
# and 80/443 (Caddy); Pocket ID is published on 127.0.0.1 only, which needs
# Docker Engine 28.0.0 or later (older ones let a neighbour on the same
# network reach a loopback-published port).
services:
  caddy:
    image: %s
    restart: unless-stopped
    network_mode: host
    env_file: .env
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - ./caddy-data:/data
      - ./caddy-config:/config
  pocket-id:
    image: %s
    restart: unless-stopped
    env_file: .env
    ports:
      - "127.0.0.1:1411:1411"
    volumes:
      - ./pocket-id-data:/app/data
  oauth2-proxy:
    image: %s
    restart: unless-stopped
    network_mode: host # it listens on 127.0.0.1:4180 itself
    env_file: .env
    depends_on:
      - pocket-id
`, caddyImage, pocketIDImage, oauth2ProxyImage)

	files["vps/Caddyfile"] = `# taskmoor's web from outside. {$DOMAIN} needs the passkey sign-in
# (Pocket ID through oauth2-proxy) before anything reaches the tunnel;
# taskmoor serve then wants its own device token and, for each dangerous
# approval, the device's passkey again.
{
	# no admin API: Caddy runs on the host's network
	admin off
}

{$AUTH_DOMAIN} {
	# until the setup is done, only AUTH_OPEN_TO reaches Pocket ID (its
	# first account becomes its administrator)
	@outside not remote_ip {$AUTH_OPEN_TO}
	respond @outside 403
	reverse_proxy 127.0.0.1:1411
}

{$DOMAIN} {
	# after the board's sign out: a page of its own, outside the sign-in,
	# so signing out does not sign straight back in
	handle /signed-out {
		header Content-Type "text/html; charset=utf-8"
		header Cache-Control no-store
		respond "<!doctype html><meta name=viewport content='width=device-width'><title>taskmoor</title><body style='font:16px -apple-system,sans-serif;margin:40px'><p>Signed out of the board.</p><p><a href='/'>Sign in again</a></p>" 200
	}
	handle /oauth2/* {
		reverse_proxy 127.0.0.1:4180 {
			header_up X-Real-IP {remote_host}
			header_up X-Forwarded-Uri {uri}
		}
	}
	handle {
		forward_auth 127.0.0.1:4180 {
			uri /oauth2/auth
			header_up X-Real-IP {remote_host}
			@unauthorized status 401
			handle_response @unauthorized {
				# the board's own requests cannot follow a sign-in to
				# another origin: they are told, and the board goes there
				@api path /api/*
				@page not path /api/*
				header @api X-Taskmoor-Login required
				respond @api 401
				redir @page /oauth2/sign_in?rd={scheme}://{host}{uri}
			}
		}
		reverse_proxy 127.0.0.1:{$TUNNEL_PORT} {
			flush_interval -1
		}
	}
}
`

	files["README.md"] = fmt.Sprintf(`# taskmoor: the web from outside

The phone opens https://%[1]s/ → Caddy on the VPS → the passkey sign-in
(Pocket ID at https://%[2]s/, through oauth2-proxy) → the reverse ssh
tunnel → taskmoor serve on the Mac (127.0.0.1:%[3]d). taskmoor then wants
the device's own token (paired once from the Mac) and, for every Accept,
Approve and allowed request, the device's passkey again.

Nothing here was installed or deployed by taskmoor: these are the steps.

## DNS

A (and AAAA) records for %[1]s and %[2]s → the VPS. Ports 80 and 443
open on the VPS (Caddy gets the certificates).

## The tunnel's user on the VPS

    sudo useradd --create-home --shell /usr/sbin/nologin %[4]s
    sudo install -d -m 700 -o %[4]s ~%[4]s/.ssh

On the Mac, a key of its own:

    ssh-keygen -t ed25519 -N "" -C taskmoor-tunnel -f %[5]s

Put vps/authorized_keys.line, with the public key (%[5]s.pub) in place of
PASTE-THE-PUBLIC-KEY-HERE, into ~%[4]s/.ssh/authorized_keys on the VPS
(mode 600, owned by %[4]s), and vps/sshd-taskmoor-tunnel.conf where sshd
reads it: /etc/ssh/sshd_config.d/ if sshd_config has
"Include /etc/ssh/sshd_config.d/*.conf" (OpenSSH 8.2 or later), else
appended at the end of /etc/ssh/sshd_config. Then check what sshd will
apply to this account — an earlier Match block, or a missing Include, can
override it:

    sudo sshd -t
    sudo sshd -T -C user=%[4]s,host=x,addr=127.0.0.1 | grep -Ei '^(gatewayports|allowtcpforwarding|permitlisten|permitopen|clientaliveinterval) '

It must print gatewayports no, allowtcpforwarding remote, permitlisten
127.0.0.1:%[6]d, permitopen none and clientaliveinterval 30; then reload
sshd. Together they allow nothing but listening on 127.0.0.1:%[6]d: no
shell, no forwarding to anything on the VPS, no public listener.

The VPS's host key: on the VPS, ssh-keygen -lf
/etc/ssh/ssh_host_ed25519_key.pub; on the Mac, the first time, in a
terminal:

    ssh -i %[5]s -o StrictHostKeyChecking=ask -N -R 127.0.0.1:%[6]d:127.0.0.1:%[3]d %[8]s

and accept only if the fingerprint it shows is that one; once it stays
connected the tunnel works (Ctrl+C). The agent below refuses an unknown
or changed key.

## The VPS's services

Docker Engine 28.0.0 or later (docker version --format
'{{.Server.Version}}'): Pocket ID is published on the VPS's loopback, which
older engines leave reachable from the same network.

Copy vps/ to the VPS (e.g. /opt/taskmoor-remote), keep .env at mode 600
(it holds this deployment's secrets: keep it, back it up with
pocket-id-data; taskmoor remote files --force makes secrets for a new
deployment, which an existing Pocket ID cannot read), then:

Set AUTH_OPEN_TO in .env to your own address first (Pocket ID's first
account becomes its administrator: nobody else may reach it before you),
then:

    docker compose up -d pocket-id caddy

Open https://%[2]s/setup, create your account and register your passkey.
Keep sign-ups off: only you have an account. In Pocket ID add an OIDC
client "taskmoor" with the callback URL https://%[1]s/oauth2/callback
(restrict it to your user or group), put its id and secret into .env
(OAUTH2_PROXY_CLIENT_ID, OAUTH2_PROXY_CLIENT_SECRET), set AUTH_OPEN_TO to
0.0.0.0/0 ::/0 (your phone signs in from anywhere), then:

    docker compose up -d --force-recreate

## The Mac

taskmoor's config.toml already has remote_host = "%[1]s" and
remote_port = %[3]d; restart taskmoor serve if it ran before. The tunnel:

    cp mac/com.taskmoor.tunnel.plist ~/Library/LaunchAgents/
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.taskmoor.tunnel.plist

Its errors go to %[7]s. To stop it:
launchctl bootout gui/$(id -u)/com.taskmoor.tunnel.

## A device

On the Mac: taskmoor device pair (or Devices on the Mac's board). On the phone: open https://%[1]s/, sign
in with your passkey, enter the code; the phone registers a passkey for
taskmoor. taskmoor device list and taskmoor device revoke ID manage them.
`, req.Domain, req.AuthDomain, req.MacPort, user, req.Key, req.VPSPort, filepath.Join(req.Store, "tunnel.log"), req.VPS)
	return files, nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
