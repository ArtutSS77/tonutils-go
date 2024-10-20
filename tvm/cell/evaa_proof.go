package cell

import (
	"log"
	"math"
	"strconv"
)

func GenerateMerkleProof(dict *Dictionary, assets map[string]*Cell) *Cell {
	return ConvertToMerkleProof(GenerateMerkleProofDirect(dict, assets))
}

func GenerateMerkleProofDirect(dict *Dictionary, assets map[string]*Cell) *Cell {
	for _, asset := range assets {
		_, err := dict.LoadValue(asset)
		if err != nil {
			log.Fatal(err)
		}
	}

	var bitsValues [][]int

	for key, asset := range assets {
		value := asset.BeginParse().MustLoadBigUInt(256)
		log.Printf("loaded value for asset %s: %s", key, value)
		log.Println(value.Bytes())
		bitsValue := bytesToBits(value.Bytes())
		bitsValues = append(bitsValues, bitsValue)
	}

	pricesSlice := dict.AsCell().BeginParse()
	return doGenerateMerkleProof("", pricesSlice, 256, bitsValues)
}

func doGenerateMerkleProof(prefix string, slice *Slice, n uint64, keys [][]int) *Cell {
	originalCell := slice.MustToCell()

	if len(keys) == 0 {
		return convertToPrunedBranch(originalCell)
	}

	bit := slice.MustLoadBoolBit()
	lb0 := 0
	if bit {
		lb0 = 1
	}
	prefixLength := uint64(0)
	pp := prefix

	if lb0 == 0 {
		prefixLength = readUnaryLength(slice)

		for i := uint64(0); i < prefixLength; i++ {
			bit := slice.MustLoadBoolBit()
			if bit {
				pp += "1"
			} else {
				pp += "0"
			}
		}

	} else {
		lb1 := 0
		bit := slice.MustLoadBoolBit()
		if bit {
			lb1 = 1
		}
		if lb1 == 0 {
			prefixLength = slice.MustLoadUInt(uint(math.Ceil(math.Log2(float64(n + 1)))))
			for i := uint64(0); i < prefixLength; i++ {
				bit := slice.MustLoadBoolBit()
				if bit {
					pp += "1"
				} else {
					pp += "0"
				}
			}
		} else {
			bit := slice.MustLoadBoolBit()
			value := "0"
			if bit {
				value = "1"
			}
			prefixLength = slice.MustLoadUInt(uint(math.Ceil(math.Log2(float64(n + 1)))))
			for i := uint64(0); i < prefixLength; i++ {
				pp += value
			}
		}
	}

	if -prefixLength == 0 {
		return originalCell
	} else {
		sl := originalCell.BeginParse()
		left := sl.MustLoadRef()
		right := sl.MustLoadRef()

		if !left.special {
			leftKeys := fetchKeys(pp, keys, "0")
			left = doGenerateMerkleProof(pp+"0", left, n-prefixLength-1, leftKeys).BeginParse()
		}
		if !right.special {
			rightKeys := fetchKeys(pp, keys, "1")
			right = doGenerateMerkleProof(pp+"1", right, n-prefixLength-1, rightKeys).BeginParse()
		}
		return BeginCell().MustStoreBuilder(sl.ToBuilder()).MustStoreRef(left.MustToCell()).MustStoreRef(left.MustToCell()).EndCell()
	}

}

func convertToPrunedBranch(c *Cell) *Cell {
	return endExoticCell(BeginCell().MustStoreUInt(1, 8).MustStoreUInt(1, 8).MustStoreBinarySnake(c.Hash()).MustStoreUInt(uint64(c.Depth(0)), 16))
}

func endExoticCell(b *Builder) *Cell {
	c := b.EndCell()
	newCell := &Cell{
		special: true,
		bitsSz:  c.bitsSz,
		refs:    c.refs,
	}
	return newCell
}

func ConvertToMerkleProof(c *Cell) *Cell {
	return endExoticCell(BeginCell().MustStoreUInt(3, 8).MustStoreBinarySnake(c.Hash(0)).MustStoreUInt(uint64(c.Depth(0)), 16).MustStoreRef(c))
}

func readUnaryLength(slice *Slice) uint64 {
	res := uint64(0)
	for {
		bit := slice.MustLoadBoolBit()
		if !bit {
			break
		}
		res++
	}
	return res
}

func bytesToBits(bytes []byte) []int {
	bits := make([]int, 0, len(bytes)*8) // Preallocate the slice with enough space for all bits

	for _, b := range bytes {
		for i := 7; i >= 0; i-- { // Iterate through each bit from MSB to LSB
			bit := (b >> i) & 1 // Shift and mask to get the bit
			bits = append(bits, int(bit))
		}
	}

	return bits
}

func fetchKeys(pp string, keys [][]int, bit string) [][]int {
	var fetchedKeys [][]int
	for _, key := range keys {
		var str string
		for _, k := range key[0 : len(pp)+1] {
			str += strconv.Itoa(k)
		}
		if str == pp+bit {
			fetchedKeys = append(fetchedKeys, key)
		}
	}
	return fetchedKeys
}
