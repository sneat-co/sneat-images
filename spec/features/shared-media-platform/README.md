---
format: https://specscore.md/feature-specification
status: Implementing
---

# Feature: Shared media platform

> [SpecScore.**Studio**](https://specscore.studio): | [Explore](https://specscore.studio/app/github.com/sneat-co/sneat-images/spec/features/shared-media-platform?op=explore) | [Edit](https://specscore.studio/app/github.com/sneat-co/sneat-images/spec/features/shared-media-platform?op=edit) | [Ask question](https://specscore.studio/app/github.com/sneat-co/sneat-images/spec/features/shared-media-platform?op=ask) | [Request change](https://specscore.studio/app/github.com/sneat-co/sneat-images/spec/features/shared-media-platform?op=request-change) |
**Status:** Implementing
**Source Ideas:** —

## Summary

Reusable DALgo-backed image upload, lifecycle, linking, and Cloudflare delivery for Sneat products.

## Problem

Sneat products currently have provider URLs and one-off image fields rather than a
shared, reusable media identity and lifecycle. That makes an image difficult to
reuse safely, makes deletion unsafe, and encourages clients or extensions to know
about Firestore, GCS, or Cloudflare details.

## Behavior

An authenticated client asks the Sneat API to begin an immutable image upload.
The media service creates a DALgo-backed `MediaAsset` in `uploading` state and
returns a short-lived GCS resumable-upload initiation URL. The client uploads
directly to GCS, reports progress, and asks the API to finalize. Finalization
streams the stored object through the blob-storage boundary to validate its
actual image type, size, dimensions, and SHA-256 before marking it `ready`.

An entity links the ready asset through `/media/{mediaID}/links/{linkID}`. The
link is authoritative for lifecycle and contains target, role, retention mode,
and normalized crop metadata. A bounded root summary carries active and
recoverable reference counts. A DALgo transaction changes the link, counts,
orphan state, and target forward reference together through a registered target
adapter. Removing a link is recoverable until its purge time; removing the last
recoverable link marks the asset orphaned but never deletes bytes immediately.

Clients use semantic variants at `https://media.sneat.co/m/{mediaID}/{variant}`.
The edge accepts only the named avatar (`avatar-xs`, `avatar-sm`, `avatar`,
`avatar-lg`), `thumbnail`, `preview`, `large`, `stream-landscape` (1920x1080) and
`stream-portrait` (1080x1920) variants. The two stream variants crop to fill,
centred, and serve video-stream backgrounds for assets uploaded with
`access: public`, fetched without credentials by the streaming compositor.
Public delivery is explicit. Private delivery requires a short-lived Ed25519
token whose audience, expiry, and user/Space scope are verified at the edge.
The access token is not part of the origin/cache identity. On a cache miss the
edge fetches the protected GCS original through the authenticated origin route;
normal image bytes are served and transformed from Cloudflare's cache.

The reference user journey is: open My profile, do nothing and still see the
current avatar or initials, choose an image, pan/zoom within a circular mask,
upload with progress, and see the new optimized avatar throughout Sneat.app.
Replacing creates a new asset. Removing leaves the user on the profile and
reveals initials with an Undo action. Good result: the user record and active
link agree, every view changes, and Back never reopens stale editor state.

The Space-contact journey starts on an existing contact detail surface. With no
action, the contact displays its linked user's global avatar or initials. Adding
an override changes only that Space context; removing it restores the dynamic
fallback. Good result: another Space continues to show its own override or the
global avatar, and independent link crops remain independent.

The Assetus and Listus journeys start on existing entity/list screens. With no
action they remain fast and show honest empty states. Adding an image yields a
thumbnail, then preview, then large view without loading the original in rows.
Good result: Assetus supports multiple ordered images and a primary preview;
Listus supports one lightweight product photo. One epilogue closes the viewer
and returns to the unchanged entity; the other replaces/removes and offers Undo,
with every actor observing the persisted result.

## Acceptance Criteria

### AC: upload-finalize

An authenticated image upload goes directly to GCS using a short-lived resumable
capability, reports progress, and cannot become ready until the stored bytes pass
server-side type, size, dimension, and hash validation.

### AC: dalgo-media-graph

Media records, links, counts, lifecycle changes, queries, and reconciliation use
DALgo. No media business logic calls Firestore directly. One immutable asset can
have multiple deterministic links and removing one cannot destroy the others.

### AC: private-edge-delivery

Named immutable variants are served through `media.sneat.co`; private requests
require an edge-verified, short-lived scoped token, and token text is excluded
from the transformed origin/cache identity.

### AC: stream-variants

`/m/{mediaID}/stream-landscape` and `/m/{mediaID}/stream-portrait` return WebP
of exactly 1920x1080 and 1080x1920 (fit cover, centred) for public assets
without a token, with public immutable cache headers.

- `fit: cover` upscales small inputs, so consumers enforce minimums: the
  videostreaming module accepts channel backgrounds only at >=1920x1080 or
  >=1080x1920 and <=4096 on the long side.
- One upload serves both orientations, so a 1920x1080 upload is scaled about
  1.78x for `stream-portrait` (softer); uploads of 3413x1920+ or square >=1920
  avoid that.
- Public variants are cached `immutable` for a year, so a removed background
  may stay fetchable from cache until evicted.

### AC: global-avatar

A user can crop, upload, replace, remove, and restore a global avatar using the
shared editor. Optimized variants become the default user representation.

### AC: contact-avatar-override

A Space contact can independently override the linked user's global avatar.
Removing the override restores the global/avatar-initials fallback.

### AC: asset-images

An authorized user can add, order, preview, replace, remove, and restore Assetus
images while list/card views request only thumbnails.

### AC: list-item-photo

An authorized user can add or capture, preview, replace, remove, and restore one
Listus item photo while list rows request only thumbnails.

### AC: recover-and-reconcile

Deleted links remain restorable for the configured window. Assets with no active
or recoverable links are marked orphaned, abandoned uploads and expired orphans
are discoverable, and cleanup is idempotent and logs missing objects/count drift.

## Open Questions

None at this time.

---
*This document follows the https://specscore.md/feature-specification*
