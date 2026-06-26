// gitevolved — the open-source local doSource client (gitevolved.ai).
//
// This module holds the four PURE, cloud-free doSource packages: operation
// (typed-operation union), projector (operation→files), conflict (semantic
// conflict DETECTION — not resolution), and export (project→stock git via
// os/exec). They run fully offline on a laptop and depend only on the Go
// standard library. The closed doSource cloud imports THIS module (inverted
// dependency: closed→open); nothing here ever reaches back into closed code —
// guaranteed by Go's cross-module internal/ visibility rule.
module github.com/do-awesome-ai/gitevolved

go 1.26.1
