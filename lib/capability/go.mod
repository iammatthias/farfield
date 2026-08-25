module github.com/iammatthias/farfield/lib/capability

go 1.27.0

require github.com/iammatthias/farfield/lib/fleet v0.0.0

// The lib/* modules are never published — resolve them from the local tree.
replace github.com/iammatthias/farfield/lib/fleet => ../fleet
