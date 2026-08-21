module github.com/iammatthias/farfield/lib/web

go 1.27.0

require (
	github.com/iammatthias/farfield/lib/auth v0.0.0
	github.com/iammatthias/farfield/lib/cid v0.0.0
	github.com/iammatthias/farfield/lib/store v0.0.0
	github.com/iammatthias/farfield/lib/theme v0.0.0
)

// The lib/* modules are never published — resolve them from the local tree.
replace (
	github.com/iammatthias/farfield/lib/auth => ../auth
	github.com/iammatthias/farfield/lib/cid => ../cid
	github.com/iammatthias/farfield/lib/store => ../store
)

replace github.com/iammatthias/farfield/lib/theme => ../../lib/theme
