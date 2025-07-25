/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysqlctl

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"

	"vitess.io/vitess/go/vt/logutil"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
)

func TestFindReplicationPosition(t *testing.T) {
	input := `MySQL binlog position: filename 'vt-0476396352-bin.000005', position '310088991', GTID of the last change '145e508e-ae54-11e9-8ce6-46824dd1815e:1-3,
	1e51f8be-ae54-11e9-a7c6-4280a041109b:1-3,
	47b59de1-b368-11e9-b48b-624401d35560:1-152981,
	557def0a-b368-11e9-84ed-f6fffd91cc57:1-3,
	599ef589-ae55-11e9-9688-ca1f44501925:1-14857169,
	b9ce485d-b36b-11e9-9b17-2a6e0a6011f4:1-371262'
	MySQL replica binlog position: master host '10.128.0.43', purge list '145e508e-ae54-11e9-8ce6-46824dd1815e:1-3, 1e51f8be-ae54-11e9-a7c6-4280a041109b:1-3, 47b59de1-b368-11e9-b48b-624401d35560:1-152981, 557def0a-b368-11e9-84ed-f6fffd91cc57:1-3, 599ef589-ae55-11e9-9688-ca1f44501925:1-14857169, b9ce485d-b36b-11e9-9b17-2a6e0a6011f4:1-371262', channel name: ''
	
	190809 00:15:44 [00] Streaming <STDOUT>
	190809 00:15:44 [00]        ...done
	190809 00:15:44 [00] Streaming <STDOUT>
	190809 00:15:44 [00]        ...done
	xtrabackup: Transaction log of lsn (405344842034) to (406364859653) was copied.
	190809 00:16:14 completed OK!`
	want := "145e508e-ae54-11e9-8ce6-46824dd1815e:1-3,1e51f8be-ae54-11e9-a7c6-4280a041109b:1-3,47b59de1-b368-11e9-b48b-624401d35560:1-152981,557def0a-b368-11e9-84ed-f6fffd91cc57:1-3,599ef589-ae55-11e9-9688-ca1f44501925:1-14857169,b9ce485d-b36b-11e9-9b17-2a6e0a6011f4:1-371262"

	pos, err := findReplicationPosition(input, "MySQL56", logutil.NewConsoleLogger())
	assert.NoError(t, err)
	assert.Equal(t, want, pos.String())
}
func TestFindReplicationPositionFromXtrabackupInfo(t *testing.T) {
	input := `tool_version = 8.0.35-30
	binlog_pos = filename 'vt-0476396352-bin.000005', position '310088991', GTID of the last change '145e508e-ae54-11e9-8ce6-46824dd1815e:1-3,
	1e51f8be-ae54-11e9-a7c6-4280a041109b:1-3,
	47b59de1-b368-11e9-b48b-624401d35560:1-152981,
	557def0a-b368-11e9-84ed-f6fffd91cc57:1-3,
	599ef589-ae55-11e9-9688-ca1f44501925:1-14857169,
	b9ce485d-b36b-11e9-9b17-2a6e0a6011f4:1-371262'
	format = xbstream
	`
	want := "145e508e-ae54-11e9-8ce6-46824dd1815e:1-3,1e51f8be-ae54-11e9-a7c6-4280a041109b:1-3,47b59de1-b368-11e9-b48b-624401d35560:1-152981,557def0a-b368-11e9-84ed-f6fffd91cc57:1-3,599ef589-ae55-11e9-9688-ca1f44501925:1-14857169,b9ce485d-b36b-11e9-9b17-2a6e0a6011f4:1-371262"

	tmp, err := os.MkdirTemp(t.TempDir(), "test")
	assert.NoError(t, err)

	f, err := os.Create(path.Join(tmp, xtrabackupInfoFile))
	assert.NoError(t, err)
	_, err = f.WriteString(input)
	assert.NoError(t, err)
	assert.NoError(t, f.Close())

	pos, err := findReplicationPositionFromXtrabackupInfo(tmp, "MySQL56", logutil.NewConsoleLogger())
	assert.NoError(t, err)
	assert.Equal(t, want, pos.String())
}

func TestFindReplicationPositionNoMatchFromXtrabackupInfo(t *testing.T) {
	// Make sure failure to find a match triggers an error.
	input := `nothing`

	_, err := findReplicationPositionFromXtrabackupInfo(input, "MySQL56", logutil.NewConsoleLogger())
	assert.Error(t, err)
}

func TestFindReplicationPositionEmptyMatchFromXtrabackupInfo(t *testing.T) {
	// Make sure failure to find a match triggers an error.
	input := `GTID of the last change '
	
	'`

	_, err := findReplicationPositionFromXtrabackupInfo(input, "MySQL56", logutil.NewConsoleLogger())
	assert.Error(t, err)
}

func TestStripeRoundTrip(t *testing.T) {
	// Generate some random input data.
	dataSize := int64(1000000)
	input := make([]byte, dataSize)
	rand.Read(input)

	test := func(blockSize int64, stripes int) {
		// Write it out striped across some buffers.
		buffers := make([]bytes.Buffer, stripes)
		readers := []io.Reader{}
		writers := []io.Writer{}
		for i := range buffers {
			readers = append(readers, &buffers[i])
			writers = append(writers, &buffers[i])
		}
		copyToStripes(writers, bytes.NewReader(input), blockSize)

		// Read it back and merge.
		outBuf := &bytes.Buffer{}
		written, err := io.Copy(outBuf, stripeReader(readers, blockSize))
		assert.NoError(t, err)
		assert.Equal(t, dataSize, written)

		output := outBuf.Bytes()
		assert.Equal(t, input, output)
	}

	// Test block size that evenly divides data size.
	test(1000, 10)
	// Test block size that doesn't evenly divide data size.
	test(3000, 10)
	// Test stripe count that doesn't evenly divide data size.
	test(1000, 30)
	// Test block size and stripe count that don't evenly divide data size.
	test(6000, 7)
}

func TestShouldDrainForBackupXtrabackup(t *testing.T) {
	be := &XtrabackupEngine{}

	// Test default behavior (should not drain)
	originalValue := xtrabackupShouldDrain
	defer func() { xtrabackupShouldDrain = originalValue }()

	xtrabackupShouldDrain = false
	assert.False(t, be.ShouldDrainForBackup(nil))
	assert.False(t, be.ShouldDrainForBackup(&tabletmanagerdatapb.BackupRequest{}))

	// Test configurable behavior (should drain when flag is set)
	xtrabackupShouldDrain = true
	assert.True(t, be.ShouldDrainForBackup(nil))
	assert.True(t, be.ShouldDrainForBackup(&tabletmanagerdatapb.BackupRequest{}))
}
