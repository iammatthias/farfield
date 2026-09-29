module github.com/iammatthias/farfield/apps/desk

go 1.27.0

require (
	github.com/hajimehoshi/ebiten/v2 v2.10.4
	github.com/iammatthias/farfield/lib/editor v0.0.0
)

require (
	github.com/ebitengine/gomobile v0.0.0-20260820040257-d11f821a26a6 // indirect
	github.com/ebitengine/hideconsole v1.0.0 // indirect
	github.com/ebitengine/purego v0.11.0 // indirect
	github.com/iammatthias/farfield/lib/wat v0.0.0 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace (
	github.com/iammatthias/farfield/lib/editor => ../../lib/editor
	github.com/iammatthias/farfield/lib/wat => ../../lib/wat
)
