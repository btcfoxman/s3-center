# S3 Center Cloudflare Connector 2.0

This local connector opens each Cloudflare identity in an independent **native Microsoft Edge or Google Chrome** profile and forces that profile through the proxy selected in S3 Center.

Version 2.0 does not launch Cloudflare with Playwright, Selenium, WebDriver, or a remote-debugging connection. Cloudflare explicitly does not support automated browsers for production challenges, and those browsers can fail login with `There was a problem with verification`. A loopback-only relay applies the per-profile proxy and optional proxy authentication. The bundled minimal Manifest V3 helper verifies the exit IP and displays the token import panel only after a concrete Cloudflare Account has been selected; it does not modify the Cloudflare login page.

## Install (Windows, current user only)

1. Install Node.js 20 or newer and an up-to-date Microsoft Edge or Google Chrome.
2. Extract this ZIP.
3. In PowerShell, run `powershell -ExecutionPolicy Bypass -File .\install.ps1`.
4. Return to S3 Center and select **Open isolated Cloudflare login**.
5. Confirm the permissions on Cloudflare's pre-filled Account API Token page. Copy the one-time API Token value, paste it into the S3 Center panel at the bottom-right of the Cloudflare page, and select **Secure import**. The connector reads the Account ID from that official page; you do not need to find or enter it.

No administrator permission is required. The installer registers the `s3center://` protocol only for the current Windows user. Browser profiles live under `%LOCALAPPDATA%\S3Center\Profiles\<profile-id>`.

If Connector 1.x was previously installed, run the new `install.ps1` once. It preserves all isolated browser profiles and login cookies while removing the old Playwright runtime.

## Security model

- S3 Center sends proxy credentials through a single-use, expiring HTTPS ticket. They are never placed in the custom protocol URL or profile metadata.
- A profile is permanently bound to one S3 Center identity and sanitized proxy endpoint. Changing an account proxy creates a new profile ID.
- The native browser helper applies the selected proxy before opening Cloudflare and verifies the browser exit IP first. HTTP/HTTPS/SOCKS5 proxies and proxy username/password authentication are supported.
- Proxy credentials remain in the Connector process memory. A random loopback-only proxy relay translates authenticated HTTP/HTTPS/SOCKS5 upstreams into a credential-free browser endpoint; credentials and the local bridge secret are not written to the profile.
- The bootstrap API token remains only in S3 Center memory. After bucket selection, S3 Center creates and encrypts a bucket-scoped R2 credential, then revokes the bootstrap token through Cloudflare's API.
- If you create the bootstrap token but abandon the flow, revoke `S3 Center temporary onboarding` manually from Cloudflare's Account API Tokens page.
- Cloudflare login cookies never leave the local profile. The connector never stores the pasted bootstrap token on disk.

## Login verification troubleshooting

1. Close any failed Cloudflare window opened by Connector 1.x.
2. Download and reinstall Connector 2.0, then start a new onboarding session. Old one-time tickets cannot be reused.
3. Keep JavaScript and cookies enabled. Do not add ad blockers, script blockers, fingerprint protection, or proxy-switching extensions to the isolated profile.
4. Use a stable proxy endpoint that does not rotate IP during login and does not intercept TLS. If direct mode succeeds but a particular proxy fails, replace that proxy; Cloudflare may challenge or reject inconsistent or low-reputation exits.

Run `uninstall.ps1` to remove the protocol registration. Profiles are retained deliberately so an accidental uninstall does not destroy signed-in sessions.
