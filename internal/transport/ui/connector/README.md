# S3 Center Cloudflare Connector

This small local connector opens each Cloudflare identity in an independent browser profile and forces that profile through the proxy selected in S3 Center.

## Install (Windows, current user only)

1. Install Node.js 20 or newer and Microsoft Edge or Google Chrome.
2. Extract this ZIP.
3. In PowerShell, run `powershell -ExecutionPolicy Bypass -File .\install.ps1`.
4. Return to S3 Center and select **Open isolated Cloudflare login**.

No administrator permission is required. The installer registers the `s3center://` protocol only for the current Windows user. Browser profiles live under `%LOCALAPPDATA%\S3Center\Profiles\<profile-id>`.

## Security model

- S3 Center sends proxy credentials through a single-use, expiring HTTPS ticket. They are never placed in the custom protocol URL or profile metadata.
- A profile is permanently bound to one S3 Center identity and sanitized proxy endpoint. Changing an account proxy creates a new profile ID.
- The connector verifies the browser exit IP with Cloudflare before opening OAuth. A failed proxy never falls back to the direct network.
- The OAuth access token remains only in S3 Center memory. After bucket selection, S3 Center creates and encrypts a bucket-scoped R2 credential.

Run `uninstall.ps1` to remove the protocol registration. Profiles are retained deliberately so an accidental uninstall does not destroy signed-in sessions.
