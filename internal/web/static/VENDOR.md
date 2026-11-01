# Vendored front-end assets

Both files are copied unmodified from unpkg and embedded with `go:embed`.
Re-download them only when bumping a version, and update this note.

| File          | Source URL                                                   | Version |
| ------------- | ------------------------------------------------------------ | ------- |
| `htmx.min.js` | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js            | 2.0.4   |
| `sse.js`      | https://unpkg.com/htmx-ext-sse@2.2.2/sse.js                  | 2.2.2   |

SHA-256 at download time:

- `htmx.min.js` `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447`
- `sse.js` `83eca6fa0611fe2b0bf1700b424b88b5eced38ef448ef9760a2ea08fbc875611`

`sse.js` is for the SSE chat in task 7.2. The list page does not load it yet.
