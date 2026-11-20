# svc

Code shared by sadbox's Go services (sprobot, sadbox.org, oncall-sync,
towngear, rproxy). Standard library only.

Only what the services genuinely share today goes here: each package
replaces code that is copied, or hand-rolled slightly differently, in
more than one of them.

## Versioning

Releases are tags `vX.Y.Z` on `main`, following semver (pre-1.0: a minor
bump may break the API). Services pin a release in their `go.mod`, like
any other dependency, and Dependabot (gomod) proposes the bumps.

The `go` line in `go.mod` is the lowest Go release the code needs, not
the services' build image, so a service on an older `go` line can still
import it.

## License

MIT, see [LICENSE](LICENSE).
