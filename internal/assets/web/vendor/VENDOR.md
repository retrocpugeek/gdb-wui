# Vendored frontend dependencies

This is the whole supply-chain story for a repository whose selling point is
`go build` and done. There is no npm, no lockfile and no bundler; instead every
vendored file is byte-identical to what the registry published, its hash is
recorded here, and `TestVendoredHashes` in `internal/assets` recomputes all of
them on every test run. A file that changes without this table changing fails
the build.

Nothing is vendored that can be avoided. There is no docking library (CSS grid
and a few pointer handlers), no icon font (inline SVG), no webfont, and no
utility libraries.

## Contents

| File | Package | Version | Bytes | sha256 |
|---|---|---|---|---|
| `xterm-6.0.0/xterm.mjs` | `@xterm/xterm` | 6.0.0 | 344970 | `b336ec65a086c056d4804b3d4c2347da5663d3f23c3f25be866467bd8857ad59` |
| `xterm-6.0.0/xterm.css` | `@xterm/xterm` | 6.0.0 | 7112 | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |
| `xterm-6.0.0/LICENSE` | `@xterm/xterm` | 6.0.0 | — | `b569f629d00f2626a8100df2a1798210535621e42164dfd426a6fe5aac7b0ccd` |
| `addon-fit-0.11.0/addon-fit.mjs` | `@xterm/addon-fit` | 0.11.0 | 1967 | `2d87e1bddc73be9111de8beee5370c3bb7aac9c94e18e6f245f02ca741ef1769` |
| `addon-fit-0.11.0/LICENSE` | `@xterm/addon-fit` | 0.11.0 | — | `e256f01188af527e4d06d21d06fbf785ae9c50d4b328bf03cbe0ba7f0aa4228f` |

Both packages are MIT licensed; the licence files are vendored alongside the
code they cover.

## Why these, and why the `.mjs` builds

**`@xterm/xterm`** renders the two terminals. The ESM build is what makes the
zero-build loop possible: verified against 6.0.0, `xterm.mjs` contains no bare
import specifiers and ends `export{Dl as Terminal}`, so a browser can load it
directly from a `<script type="module">` with no import map and no bundler.

**`@xterm/addon-fit`** sizes the terminal to its container. Hand-rolling it
means depending on the same private xterm internals it does, with none of the
maintenance.

The 1.7 MB `xterm.mjs.map` is deliberately **not** vendored. The consequence is
a 404 for the sourcemap in devtools, which is a fair trade for not shipping a
1.7 MB file in the binary. The `//# sourceMappingURL` comment is left in place
because the files are vendored byte-identically and the hash table above says
so.

## Refetching

No npm required:

```sh
cd $(mktemp -d)
curl -sSL -O https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz
curl -sSL -O https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz

# Tarball hashes, for reference:
#   908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6  xterm-6.0.0.tgz
#   26003b4517a132b64e4ff228fd88a5fda3fff5e606c76093f6dcff772e9ecec0  addon-fit-0.11.0.tgz
sha256sum *.tgz

mkdir xt af && tar -C xt -xzf xterm-6.0.0.tgz && tar -C af -xzf addon-fit-0.11.0.tgz

V=internal/assets/web/vendor
cp xt/package/lib/xterm.mjs      "$V/xterm-6.0.0/"
cp xt/package/css/xterm.css      "$V/xterm-6.0.0/"
cp xt/package/LICENSE            "$V/xterm-6.0.0/"
cp af/package/lib/addon-fit.mjs  "$V/addon-fit-0.11.0/"
cp af/package/LICENSE            "$V/addon-fit-0.11.0/"
```

Then update the table above with the new versions and hashes:

```sh
find internal/assets/web/vendor -type f ! -name VENDOR.md | sort | xargs sha256sum
```
