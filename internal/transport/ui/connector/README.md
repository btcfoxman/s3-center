# S3 Center Cloudflare Connector 2.1

This local connector opens each Cloudflare identity in an independent **native Google Chrome** profile and forces that profile through the proxy selected in S3 Center.

Version 2.1 does not use the machine's default browser profile or Microsoft Edge. Edge can implicitly sign a new profile into the Windows account, so Connector 2.1 starts from a fresh unsigned Chrome-only data directory, disables sync, first-run import, extensions, and password storage, and clearly names the profile `S3 Center <id>`. It also does not launch Cloudflare with Playwright, Selenium, WebDriver, or a remote-debugging connection. A loopback-only relay applies the per-profile proxy and optional proxy authentication. A random loopback control page verifies the exit IP and accepts the one-time Token after the administrator creates it on Cloudflare; no extension modifies or observes the Cloudflare login page.

## Install (Windows, current user only)

1. Install Node.js 20 or newer and an up-to-date Google Chrome.
2. Extract this ZIP.
3. In PowerShell, run `powershell -ExecutionPolicy Bypass -File .\install.ps1`.
4. Return to S3 Center and select **Open isolated Cloudflare login**.
5. In the S3 Center local control tab, select **Open Cloudflare official page**. Confirm the permissions on the pre-filled Account API Token page. Copy the one-time API Token value and the current Cloudflare Account page URL back into the local control tab, then select **Secure import**. The connector extracts the Account ID from the URL.

No administrator permission is required. The installer registers the `s3center://` protocol only for the current Windows user. Active browser profiles live under `%LOCALAPPDATA%\S3Center\Profiles\<profile-id>\browser-chrome-v1`.

If an older Connector was previously installed, run the new `install.ps1` once. Legacy profile directories are retained for recovery but are never opened by 2.1; sign in to Cloudflare again in the fresh Chrome profile.

## Security model

- S3 Center sends proxy credentials through a single-use, expiring HTTPS ticket. They are never placed in the custom protocol URL or profile metadata.
- A profile is permanently bound to one S3 Center identity and sanitized proxy endpoint. Changing an account proxy creates a new profile ID. Connector 2.1 also uses a new Chrome-only profile generation so local Edge identity data cannot be inherited.
- The native Chrome helper applies the selected proxy before opening Cloudflare and verifies the browser exit IP first. HTTP/HTTPS/SOCKS5 proxies and proxy username/password authentication are supported.
- Proxy credentials remain in the Connector process memory. A random loopback-only proxy relay translates authenticated HTTP/HTTPS/SOCKS5 upstreams into a credential-free browser endpoint; credentials and the local bridge secret are not written to the profile.
- The bootstrap API token crosses only the random loopback control page and the session's HTTPS report endpoint, then remains only in S3 Center memory. After bucket selection, S3 Center creates and encrypts a bucket-scoped R2 credential, then revokes the bootstrap token through Cloudflare's API.
- If you create the bootstrap token but abandon the flow, revoke `S3 Center temporary onboarding` manually from Cloudflare's Account API Tokens page.
- Cloudflare login cookies never leave the local profile. The connector never stores the pasted bootstrap token or Account page URL on disk.

## Login verification troubleshooting

1. Close every Cloudflare window opened by an older Connector.
2. Download and reinstall Connector 2.1, then start a new onboarding session. Old one-time tickets cannot be reused.
3. Keep JavaScript and cookies enabled. Connector disables extensions in the isolated profile so blockers, fingerprint tools, or proxy-switching extensions cannot interfere with verification.
4. Use a stable proxy endpoint that does not rotate IP during login and does not intercept TLS. If direct mode succeeds but a particular proxy fails, replace that proxy; Cloudflare may challenge or reject inconsistent or low-reputation exits.

Run `uninstall.ps1` to remove the protocol registration. Profiles are retained deliberately so an accidental uninstall does not destroy data; Connector 2.1 only opens its Chrome-specific profile generation.
