module github.com/iammatthias/farfield/apps/dead-presidents

go 1.25.0

require (
	github.com/iammatthias/farfield/lib/store v0.0.0
	github.com/iammatthias/farfield/lib/theme v0.0.0
	github.com/iammatthias/farfield/lib/web v0.0.0
)

require (
	github.com/iammatthias/farfield/lib/auth v0.0.0 // indirect
	github.com/iammatthias/farfield/lib/cid v0.0.0 // indirect
)

// The lib/* modules are never published — resolve them from the local tree.
replace (
	github.com/iammatthias/farfield/lib/auth => ../../lib/auth
	github.com/iammatthias/farfield/lib/cid => ../../lib/cid
	github.com/iammatthias/farfield/lib/store => ../../lib/store
	github.com/iammatthias/farfield/lib/theme => ../../lib/theme
	github.com/iammatthias/farfield/lib/web => ../../lib/web
)
