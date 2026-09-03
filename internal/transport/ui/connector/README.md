# S3 Center Cloudflare Connector

This small local connector opens each Cloudflare identity in an independent browser profile and forces that profile through the proxy selected in S3 Center.

## Install (Windows, current user only)

1. Install Node.js 20 or newer and Microsoft Edge or Google Chrome.
2. Extract this ZIP.
3. In PowerShell, run `powershell -ExecutionPolicy Bypass -File .\install.ps1`.
4. Return to S3 Center and select **Open isolated Cloudflare login**.
5. Confirm the permissions on Cloudflare's pre-filled Account API Token page. Copy the one-time API Token value, paste it into the S3 Center panel at the bottom-right of the Cloudflare page, and select **Secure import**. The connector reads the Account ID from that official page; you do not need to find or enter it.

No administrator permission is required. The installer registers the `s3center://` protocol only for the current Windows user. Browser profiles live under `%LOCALAPPDATA%\S3Center\Profiles\<profile-id>`.

## Security model

- S3 Center sends proxy credentials through a single-use, expiring HTTPS ticket. They are never placed in the custom protocol URL or profile metadata.
- A profile is permanently bound to one S3 Center identity and sanitized proxy endpoint. Changing an account proxy creates a new profile ID.
- The connector verifies the browser exit IP with Cloudflare before opening the official token page. A failed proxy never falls back to the direct network.
- The bootstrap API token remains only in S3 Center memory. After bucket selection, S3 Center creates and encrypts a bucket-scoped R2 credential, then revokes the bootstrap token through Cloudflare's API.
- If you create the bootstrap token but abandon the flow, revoke `S3 Center temporary onboarding` manually from Cloudflare's Account API Tokens page.
- Cloudflare login cookies never leave the local profile. The connector never stores the pasted bootstrap token on disk.

Run `uninstall.ps1` to remove the protocol registration. Profiles are retained deliberately so an accidental uninstall does not destroy signed-in sessions.
