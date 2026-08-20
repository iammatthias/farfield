module github.com/iammatthias/farfield/lib/markdown

go 1.27.0

require (
	github.com/iammatthias/farfield/lib/recipe v0.0.0
	github.com/yuin/goldmark v1.8.2
)

require gopkg.in/yaml.v3 v3.0.1 // indirect

// The lib/* modules are never published — resolve them from the local tree.
replace github.com/iammatthias/farfield/lib/recipe => ../recipe
