package ipod

// Device-type detection from the model string stored on the device. The stock
// firmware writes the hardware model number (e.g. "MB147") into the
// iPod_Control/Device/SysInfo file under the "ModelNumStr" key. The model
// number alone is enough to tell a 5G/5.5G Video iPod (dbv 0x0f, no HASH58,
// needs a 0x68-byte mhbd header) from a 6G/7G Classic (dbv 0x19, 0xF4 header,
// HASH58): no hardware query is required.

// classicModels lists the known iPod Classic (6G/7G, A1238 "N25") model
// numbers. Every other hard-drive Classic-era model is a 5G/5.5G Video iPod.
// Source: The Apple Wiki "Models/iPod" table.
var classicModels = map[string]bool{
	"MB029": true, // 6G 80GB silver
	"MB145": true, // 6G 160GB silver
	"MB147": true, // 6G 80GB black
	"MB150": true, // 6G 160GB black
	"MB562": true, // 7G 120GB silver
	"MB565": true, // 7G 120GB gray
	"MC293": true, // 7G 160GB silver (Late 2009)
	"MC297": true, // 7G 160GB gray (Late 2009)
}

// IsClassic reports whether the given model number denotes an iPod Classic
// (6G/7G), i.e. one that needs the 0x19/HASH58 format. Unknown model numbers
// are treated as the older 5G/5.5G Video format, which is the safe default
// when the SysInfo file is missing or unrecognized.
func IsClassic(model string) bool {
	return classicModels[model]
}
