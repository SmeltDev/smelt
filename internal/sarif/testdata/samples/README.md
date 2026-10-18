These are NOT captures. ruff and eslint were not installable where this
package was written, so the two files are hand-written in the shape those tools
document: Ruff reports absolute `file://` URIs, ESLint's formatter reports
relative URIs, a `rule.id`-less result and a suppressed result. Replace them
with real captures (see ../real) when the tools are at hand.
