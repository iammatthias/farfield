module github.com/iammatthias/farfield/apps/library

go 1.25.0

require (
	github.com/iammatthias/farfield/lib/auth v0.0.0
	github.com/iammatthias/farfield/lib/cid v0.0.0
	github.com/iammatthias/farfield/lib/pulse v0.0.0
	github.com/iammatthias/farfield/lib/store v0.0.0
	github.com/iammatthias/farfield/lib/theme v0.0.0
	github.com/iammatthias/farfield/lib/web v0.0.0
	golang.org/x/image v0.40.0
	modernc.org/sqlite v1.50.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// The lib/* modules are never published — resolve them from the local tree.
replace (
	github.com/iammatthias/farfield/lib/auth => ../../lib/auth
	github.com/iammatthias/farfield/lib/cid => ../../lib/cid
	github.com/iammatthias/farfield/lib/pulse => ../../lib/pulse
	github.com/iammatthias/farfield/lib/store => ../../lib/store
	github.com/iammatthias/farfield/lib/theme => ../../lib/theme
	github.com/iammatthias/farfield/lib/web => ../../lib/web
)
