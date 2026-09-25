// Package httpapix stands for an UNRELATED package whose own import path
// merely shares httpapi's own characters as a STRING PREFIX -- round 4
// review of PR #324, finding S9: allowedPrefixes' own matching loop
// (isAllowed) calls matchesPathOrSubpackage, which requires an exact
// match or a "prefix + /" boundary, exactly like isTargetPackage already
// does for mcpgateway (that fixture's own doc comment) -- but nothing in
// this package's own testdata previously exercised that same boundary
// for the allowedPrefixes loop itself. A mutant that inlines a bare
// strings.HasPrefix(path, prefix) there (dropping the "+/") would falsely
// allow this package's own import below, since
// ".../inbound/httpapix" does start with the literal characters
// ".../inbound/httpapi".
package httpapix

type Client struct{}
