package lcd

import (
	"encoding/binary"
	"image"
	"image/color"
	"time"

	"../cpu"
	"../io"
	"../mmu"
	"../timer"
	"github.com/hajimehoshi/ebiten"
)

type GBLCD struct {
	// 160*144 screen size, where each xy can be an RGBA value
	screen      [23040]color.RGBA
	mode        uint8
	tileset     [2]byte
	modeClock   int16
	currentLine uint16
	window      *image.RGBA
	currentBG   *image.RGBA
}

type Pixel struct {
	Point image.Point
	Color color.RGBA
}

type Sprite struct {
	Y    int
	X    int
	Tile []*Pixel
}

const (
	LCDC = 0xFF40
	STAT = 0xFF41
	SCY  = 0xFF42
	SCX  = 0xFF43
	LY   = 0xFF44
	LYC  = 0xFF45
)

func (gblcd *GBLCD) InitLCD() {
	gblcd.mode = 2
}

const maxCycles = 69905
const tileBytes = 16

var (
	GbMMU   *mmu.GBMMU
	GbCPU   *cpu.GBCPU
	GbTimer *timer.GBTimer
	GbIO    *io.GBIO

	frames = 0
	second = time.Tick(time.Second)
)

// Set offsets to uint8s, just add and let it overflow
// Linear offset in bg map of first tile in window
// ((y offset * num pixels per row) + (x offset * 8)) / 8
func (gblcd *GBLCD) Run(screen *ebiten.Image) error {
	// Logical update
	GbIO.ReadInput()
	gblcd.Update(screen)

	gblcd.renderWindow()

	ebitenBG, _ := ebiten.NewImageFromImage(gblcd.window, ebiten.FilterDefault)
	opts := &ebiten.DrawImageOptions{}
	screen.DrawImage(ebitenBG, opts)

	// Graphics update

	return nil
}

func (gblcd *GBLCD) Update(screen *ebiten.Image) {
	// Main loop
	// 1. Execute next operation
	// 2. Update total cycles
	// 3. Update timers
	// 4. Update LCD
	// 5. Perform interrupts
	updateCycles := 0

	// 4194304 is max cycles that can be executed per second
	// Since running at 60 FPS, each cycle max must be 4194304/60 = 69905
	for updateCycles < maxCycles {
		if !GbCPU.Halted {
			if GbCPU.EIReceived {
				GbCPU.IME = 1
			}
			GbCPU.Jumped = false
			opcode := GbCPU.Regs.PC[:]
			opcodeInt := binary.LittleEndian.Uint16(opcode)

			operation := GbMMU.Memory[opcodeInt]

			// fmt.Printf("%02X:%02X\t%02X\t%v\n", opcode[1], opcode[0], operation, GbCPU.Instrs[operation])
			// fmt.Printf("CTRL: %02X, STAT: %02X\n", GbMMU.Memory[0xFF40], GbMMU.Memory[0xFF41])
			// fmt.Printf("IE: %02X, LY: %02X, LYC: %02X\n", GbMMU.Memory[0xFFFE], GbMMU.Memory[0xFF44], GbMMU.Memory[0xFF45])
			// fmt.Println(GbMMU.Memory[0xFF07])

			delay := GbCPU.Instrs[operation].Executor()

			// Update cycles
			updateCycles += int(GbCPU.Instrs[operation].TCycles) + delay

			// Update graphics
			gblcd.updateGraphics(int(GbCPU.Instrs[operation].TCycles)+delay, screen)

			GbTimer.Increment(updateCycles)

			if GbCPU.Jumped {
				continue
			} else {
				nextInstr := binary.LittleEndian.Uint16(GbCPU.Regs.PC) + GbCPU.Instrs[operation].NumOperands
				// Interesting problem if we don't make a new byte array here
				// TODO Explain exactly what it is... when I understand it
				nextInstrAdddr := make([]byte, 2)
				binary.LittleEndian.PutUint16(nextInstrAdddr, nextInstr)
				GbCPU.Regs.PC = nextInstrAdddr
			}

			if GbCPU.IME != 0 && GbMMU.Memory[0xFFFE] != 0 && GbMMU.Memory[0xFF0F] != 0 {
				interrupt := GbMMU.Memory[0xFFFE] & GbMMU.Memory[0xFF0F]

				if interrupt&1 != 0 {
					// Run interrupt handler
					GbCPU.RST40()

					// Set VBlank bit
					GbMMU.Memory[0xFF0F] ^= 1
					updateCycles += 16
				} else if interrupt&4 != 0 {
					GbCPU.RST50()
					GbMMU.Memory[0xFF0F] ^= 2
				}
			}

			GbTimer.Increment(updateCycles)
		} else {
			currentIF := GbMMU.ReadByte(0xFF0F)

			if currentIF != GbCPU.IFPreHalt {
				GbCPU.Halted = false
			}

			if GbCPU.IME != 0 && GbMMU.Memory[0xFFFE] != 0 && GbMMU.Memory[0xFF0F] != 0 {
				interrupt := GbMMU.Memory[0xFFFE] & GbMMU.Memory[0xFF0F]

				if interrupt&1 != 0 {
					// Run interrupt handler
					GbCPU.RST40()

					// Set VBlank bit
					GbMMU.Memory[0xFF0F] ^= 1
					updateCycles += 16
				} else if interrupt&4 != 0 {
					GbCPU.RST50()
					GbMMU.Memory[0xFF0F] ^= 2
				}
			}

			updateCycles++
			GbTimer.Increment(updateCycles)
		}
	}
}

func (gblcd *GBLCD) updateGraphics(cycles int, screen *ebiten.Image) {
	if lcdEnabled() == 0 {
		gblcd.modeClock = 0
		gblcd.currentLine = 0
		GbMMU.Memory[LY] = 0

		// Clear LCD status
		GbMMU.Memory[STAT] = 0x80
	} else {
		gblcd.modeClock += int16(cycles)
		gblcd.setLCDStatus(screen)
	}
}

func lcdEnabled() byte {
	return GbMMU.Memory[LCDC] & (1 << 7)
}

func (gblcd *GBLCD) renderWindow() {
	bgmap := GbMMU.Memory[0x9800:0x9C00]

	// ((y tile offset * num pixels per row) + (x tile offset * 8)) / 8
	window := image.NewRGBA(image.Rect(0, 0, 160, 144))
	var tiles [][]*Pixel

	var yVal byte = GbMMU.Memory[SCY]
	var xVal byte = GbMMU.Memory[SCX]
	var initialX byte = GbMMU.Memory[SCX]
	yOff := int(yVal) * 256
	xOff := int(xVal) * 8
	offset := (yOff + xOff) / 64

	// Get tiles on background map
	for height := 0; height < 18; height++ {
		for width := 0; width < 20; width++ {
			// Pass tile ID to renderTile
			tile := renderTile(int(bgmap[offset]), false)
			tiles = append(tiles, tile)

			// Move to the next tile
			xVal += 8
			xOff = int(xVal) * 8

			offset = (yOff + xOff) / 64
		}

		yVal += 8
		yOff = int(yVal) * 256
		xVal = initialX
		xOff = int(xVal) * 8
		offset = (yOff + xOff) / 64
	}

	sprites := gblcd.renderSprites()

	for i, tile := range tiles {
		for _, px := range tile {
			tileX := ((i % 20) * 8)
			tileY := ((i / 20) * 8)
			window.Set(px.Point.X+tileX, px.Point.Y+tileY, px.Color)
		}
	}

	// fmt.Println(sprites[6].Y)
	for _, sprite := range sprites {
		for _, px := range sprite.Tile {
			window.Set(px.Point.X+sprite.X, px.Point.Y+sprite.Y, px.Color)
		}
	}

	gblcd.window = window
}

func (gblcd *GBLCD) renderSprites() []*Sprite {
	var sprites []*Sprite
	oam := GbMMU.Memory[0xFE00:0xFEA0]

	for i := 0; i < len(oam); i += 4 {
		// Get next sprite data
		spriteData := GbMMU.Memory[0xFE00+i : 0xFE00+i+4]

		yLoc := spriteData[0] - 16
		xLoc := spriteData[1] - 8
		tile := renderTile(int(spriteData[2]), true)

		s := &Sprite{
			Y:    int(yLoc),
			X:    int(xLoc),
			Tile: tile,
		}

		sprites = append(sprites, s)
	}

	return sprites
}

func renderTile(tileID int, sprites bool) []*Pixel {
	pixels := []*Pixel{}

	palette := [4]color.RGBA{
		color.RGBA{205, 255, 205, 255},
		color.RGBA{120, 170, 120, 255},
		color.RGBA{35, 85, 35, 255},
		color.RGBA{0, 0, 0, 255},
	}

	loTiles := GbMMU.Memory[LCDC]&(1<<4) != 0

	if loTiles || sprites {
		tileID = 0x8000 + (tileID * 16)
	} else {
		// If we're in hi tiles set, tile locations are signed
		if tileID > 127 {
			tileID = tileID - 128
			tileID = 0x8800 + (tileID * 16)
		} else {
			tileID = 0x8800 + ((tileID + 128) * 16)
		}
	}

	tileVals := GbMMU.Memory[tileID : tileID+16]

	// Iterate over lines of tiles, represented by 2 bytes
	for line := 0; line < 8; line++ {
		hi := tileVals[line*2]
		lo := tileVals[line*2+1]

		// Iterate over individual pixels of tile lines
		for pix := 0; pix < 8; pix++ {
			// TODO Maybe make color lookup more like hardware
			// http://www.codeslinger.co.uk/pages/projects/gameboy/graphics.html
			hiBit := (lo >> (7 - uint8(pix))) & 1
			loBit := (hi >> (7 - uint8(pix))) & 1

			colorIndex := loBit + hiBit*2
			color := palette[colorIndex]
			pixX := pix
			pixY := line

			p := &Pixel{
				Point: image.Point{pixX, pixY},
				Color: color,
			}

			pixels = append(pixels, p)

			// tile.Set(pixX, pixY, color)
		}
	}

	// return tile
	return pixels
}

func (gblcd *GBLCD) setLCDStatus(screen *ebiten.Image) {
	switch gblcd.mode {
	// HBlank
	case 0:
		if gblcd.modeClock >= 204 {
			gblcd.modeClock = 0
			gblcd.currentLine++
			GbMMU.Memory[LY]++

			if gblcd.currentLine == 143 {
				gblcd.mode = 1
				GbMMU.Memory[STAT] |= (1 << 0)
				GbMMU.Memory[STAT] &^= (1 << 1)

				// VBlank interrupt
				GbMMU.Memory[0xFF0F] |= 1
			} else {
				GbMMU.Memory[STAT] &^= (1 << 0)
				GbMMU.Memory[STAT] |= (1 << 1)
				gblcd.mode = 2
			}
		}
	// VBlank
	case 1:
		if gblcd.modeClock >= 456 {
			gblcd.modeClock = 0
			gblcd.currentLine++
			GbMMU.Memory[LY]++

			if gblcd.currentLine > 153 {
				GbMMU.Memory[STAT] &^= (1 << 0)
				GbMMU.Memory[STAT] |= (1 << 1)
				gblcd.mode = 2
				GbMMU.Memory[LY] = 0
				gblcd.currentLine = 0
			}
		}
	// OAM read mode
	case 2:
		if gblcd.modeClock >= 80 {
			gblcd.modeClock = 0
			GbMMU.Memory[STAT] |= (1 << 0)
			GbMMU.Memory[STAT] |= (1 << 1)
			gblcd.mode = 3
		}
	// VRAM read mode
	case 3:
		if gblcd.modeClock >= 172 {
			gblcd.modeClock = 0
			GbMMU.Memory[STAT] &^= (1 << 0)
			GbMMU.Memory[STAT] &^= (1 << 1)
			gblcd.mode = 0

			// TODO Write scanline to framebuffer
		}
	}

	if GbMMU.Memory[LY] == GbMMU.Memory[LYC] {
		// Set coincidence bit
		GbMMU.Memory[STAT] |= (1 << 2)
	} else {
		// Clear coincidence bit
		GbMMU.Memory[STAT] &^= (1 << 2)
	}
}
