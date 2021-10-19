package test

import (
	"encoding/binary"
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	"go.vocdoni.io/dvote/census"
	"go.vocdoni.io/dvote/db"
	"go.vocdoni.io/dvote/test/testcommon/testutil"
	"go.vocdoni.io/dvote/vochain"
	"go.vocdoni.io/dvote/vochain/censusdownloader"
	models "go.vocdoni.io/proto/build/go/models"
)

func TestRollingCensus(t *testing.T) {
	rng := testutil.NewRandom(0)
	app := vochain.TestBaseApplication(t)

	var cm census.Manager
	qt.Assert(t, cm.Start(db.TypePebble, t.TempDir(), ""), qt.IsNil)
	defer func() { qt.Assert(t, cm.Stop(), qt.IsNil) }()

	_ = censusdownloader.NewCensusDownloader(app, &cm, false)

	pid := rng.RandomBytes(32)

	// Block 1
	app.State.Rollback()
	app.State.SetHeight(1)

	censusURI := "ipfs://foobar"
	p := &models.Process{
		EntityId:   rng.RandomBytes(32),
		CensusURI:  &censusURI,
		ProcessId:  pid,
		StartBlock: 3,
		Mode: &models.ProcessMode{
			PreRegister: true,
		},
		EnvelopeType: &models.EnvelopeType{
			Anonymous: true,
		},
	}
	qt.Assert(t, app.State.AddProcess(p), qt.IsNil)

	_, err := app.State.Save()
	qt.Assert(t, err, qt.IsNil)

	// Block 2
	app.State.Rollback()
	app.State.SetHeight(2)

	const numKeys = 128
	keys := make([][]byte, numKeys)
	for i := 0; i < numKeys; i++ {
		// Create a key with the last byte at 0 to make
		// sure it fits in the Poseidon field
		key := [32]byte{}
		copy(key[:31], rng.RandomBytes(31))
		keys[i] = key[:]
		qt.Assert(t, app.State.AddToRollingCensus(pid, keys[i], nil), qt.IsNil)
	}

	_, err = app.State.Save()
	qt.Assert(t, err, qt.IsNil)

	// Block 3
	app.State.Rollback()
	app.State.SetHeight(3)

	process, err := app.State.Process(pid, true)
	qt.Assert(t, err, qt.IsNil)
	censusID := fmt.Sprintf("%x", process.CensusRoot)
	census, ok := cm.Trees[censusID]
	qt.Assert(t, ok, qt.Equals, true)

	for i, key := range keys {
		indexLE, err := cm.KeyToIndex(censusID, key)
		qt.Assert(t, err, qt.IsNil)
		censusKey, err := census.Get(indexLE)
		qt.Assert(t, err, qt.IsNil)
		qt.Assert(t, censusKey, qt.DeepEquals, key)
		index := binary.LittleEndian.Uint64(indexLE)
		qt.Assert(t, index, qt.Equals, uint64(i))
	}

	_, err = app.State.Save()
	qt.Assert(t, err, qt.IsNil)
}
