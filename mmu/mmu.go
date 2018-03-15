package mmu

import (
	"fmt"
	"io/ioutil"
	"../cartcon"
)

// Reference http://gameboy.mongenel.com/dmg/asmmemmap.html
type GBMMU struct {
	// Bootstrap ROM
	bios	[256]byte
	Cart	cartcon.Cartridge
	// Bank 0 not switchable in DMG and CGB
	// For CGB, bank 1 is switchable
	ram		[2][4096]byte
	// Reserved
	echoram	[7680]byte
}

func (gbmmu *GBMMU) InitMMU() {
	gbmmu.bios = BootstrapROM
}

// Reads cartridge ROM into memory
// Returns ROM as byte slice
func (gbmmu *GBMMU) LoadCart(path string) error {
	cartData, err := ioutil.ReadFile(path)
	if err != nil {
		return fmt.Errorf("MMU: loadCart(%s) failed: %s", path, err)
	}
	
	// Cartridge header layout
	// http://gbdev.gg8.se/wiki/articles/The_Cartridge_Header
	cart := new(cartcon.Cartridge)
	cart.MBC = cartData
	cart.Title = string(cart.MBC[0x0134:0x0143])
	cart.CGBFlag = int(cart.MBC[0x0143])
	cart.Type = int(cart.MBC[0x0147])
	cart.ROMSize = int(cart.MBC[0x0148])
	cart.RAMSize = int(cart.MBC[0x0149])
	
	gbmmu.Cart = *cart
	return nil
}
