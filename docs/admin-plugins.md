# Manage server plugins and updates

Open **Plugins** in the Duckway admin sidebar (`/admin/plugins`). The page uses
an administrator session and separates the version published for clients from
a different version available at the saved manifest source. It shows description,
source URL, check times and errors, and each release's platform artifacts with
byte sizes and SHA-256 digests. Expand **Platforms and integrity** to inspect
artifacts. **Refresh** reloads the server catalog.

Enter a name and trusted manifest URL, then choose **Register and check**. Use
an HTTP(S) URL without credentials, query parameters, or fragments. The source
URL is saved and visible to administrators. The server's configured origin
policy must permit it; an existing name keeps its fixed source URL. Registration
checks the manifest and leaves a new plugin unpublished and disabled.

The server automatically checks registered sources every six hours by default.
**Check now** also checks the saved source. A check discovers release metadata;
it does not publish a release or update installed clients. Check failures leave
the published release intact, and the page shows the last successful check time
alongside the latest attempt and error.

Review the available version and choose **Publish available version** to confirm
that exact content-hash version. Publishing downloads all declared artifacts,
verifies their sizes and hashes, then enables distribution on success. If the
candidate changes or verification fails, publication fails and preserves the
existing published release. Integrity checks do not establish whether native
code is safe: publishing a source is an administrator trust decision.

**Enable** distributes the already published release; **Disable** blocks future
distribution. Neither action stops or removes plugins already installed on
clients. Publication never forces a client update. A client connected to this
Duckway server explicitly installs or updates the approved release with:

```sh
duckway plugin install NAME
duckway plugin update NAME
```

Replace `NAME` with the registered name, for example `duckllo`.

**Client versions** loads actual server-received version and update reports for
known clients. It shows the installed and published hashes, last-seen time,
last update attempt, and one of: **Not reported**, **Stale report**, **Not
installed**, **Up to date**, **Different version**, **Update failed**, or **Not
published**. Reports older than 24 hours are stale. Different hashes do not
establish semantic version order; the page does not label an unknown version
as newer or older. Reports are snapshots and do not reveal running processes.
Explicit local development packages are not imported into this catalog or shown
in fleet reports; installing one does not change the Duckway server.

The page uses same-origin session-authenticated JSON APIs: `GET /api/plugins`,
`POST /api/plugins/import` (`name`, `manifest_url`), `POST
/api/plugins/NAME/check` (empty JSON object), `POST /api/plugins/NAME/publish`
(`version`: expected available hash), `POST /api/plugins/NAME/enable` or
`/disable` (empty JSON object), and `GET /api/plugins/NAME/clients`. It prevents
duplicate submissions while requests are active and keeps errors visible inline.
After an ambiguous failure, refresh to confirm server state before retrying.

Publication sends both the captured available version and its server-computed whole-manifest digest. Refresh and review again if either changes; publisher version reuse does not preserve approval.
