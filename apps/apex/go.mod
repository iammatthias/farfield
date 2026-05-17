module github.com/iammatthias/farfield/apps/apex

go 1.25.0

require github.com/iammatthias/farfield/lib/store v0.0.0

// The lib/* modules are never published — resolve them from the local tree.
replace github.com/iammatthias/farfield/lib/store => ../../lib/store
