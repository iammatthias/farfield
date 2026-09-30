module github.com/iammatthias/farfield/lib/editor

go 1.27.0

require (
	github.com/iammatthias/farfield/lib/wat v0.0.0
	github.com/tetratelabs/wazero v1.12.0
	golang.org/x/image v0.40.0
)

require (
	github.com/iammatthias/farfield/lib/cid v0.0.0
	github.com/iammatthias/farfield/lib/theme v0.0.0
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/iammatthias/farfield/lib/wat => ../wat

replace github.com/iammatthias/farfield/lib/theme => ../theme

replace github.com/iammatthias/farfield/lib/cid => ../cid
