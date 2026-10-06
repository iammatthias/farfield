module github.com/iammatthias/farfield/apps/keys

go 1.27.0

require (
	github.com/descope/virtualwebauthn v1.0.5
	github.com/go-webauthn/webauthn v0.18.2
	github.com/iammatthias/farfield/lib/keys v0.0.0
	github.com/iammatthias/farfield/lib/pulse v0.0.0
	github.com/iammatthias/farfield/lib/store v0.0.0
	github.com/iammatthias/farfield/lib/theme v0.0.0
	github.com/iammatthias/farfield/lib/web v0.0.0
	modernc.org/sqlite v1.50.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fxamacker/cbor/v2 v2.9.4 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.3.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/iammatthias/farfield/lib/auth v0.0.0 // indirect
	github.com/iammatthias/farfield/lib/cid v0.0.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// The lib/* modules are never published — resolve them from the local tree.
replace (
	github.com/iammatthias/farfield/lib/auth => ../../lib/auth
	github.com/iammatthias/farfield/lib/cid => ../../lib/cid
	github.com/iammatthias/farfield/lib/keys => ../../lib/keys
	github.com/iammatthias/farfield/lib/pulse => ../../lib/pulse
	github.com/iammatthias/farfield/lib/store => ../../lib/store
	github.com/iammatthias/farfield/lib/theme => ../../lib/theme
	github.com/iammatthias/farfield/lib/web => ../../lib/web
)
