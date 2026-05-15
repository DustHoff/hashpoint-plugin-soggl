package main

import (
	sdk "github.com/dusthoff/hashpoint/plugin/sdk"

	"github.com/dusthoff/hashpoint-plugin-soggl/internal/plugin"
)

func main() {
	sdk.Serve(plugin.New())
}
