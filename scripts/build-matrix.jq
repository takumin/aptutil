{
	include: [
		.[] | split("/") | {goos: .[0], goarch: .[1]}
	]
}
