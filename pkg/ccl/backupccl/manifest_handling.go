// Copyright 2016 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package backupccl

import (
	"bytes"
	"context"
	"io/ioutil"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/storage/cloud"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

const (
	// BackupDescriptorName is the file name used for serialized
	// BackupDescriptor protos.
	BackupDescriptorName = "BACKUP"
	// BackupManifestName is a future name for the serialized
	// BackupDescriptor proto.
	BackupManifestName = "BACKUP_MANIFEST"

	// BackupPartitionDescriptorPrefix is the file name prefix for serialized
	// BackupPartitionDescriptor protos.
	BackupPartitionDescriptorPrefix = "BACKUP_PART"
	// BackupDescriptorCheckpointName is the file name used to store the
	// serialized BackupDescriptor proto while the backup is in progress.
	BackupDescriptorCheckpointName = "BACKUP-CHECKPOINT"
	// BackupFormatDescriptorTrackingVersion added tracking of complete DBs.
	BackupFormatDescriptorTrackingVersion uint32 = 1
)

// BackupFileDescriptors is an alias on which to implement sort's interface.
type BackupFileDescriptors []BackupDescriptor_File

func (r BackupFileDescriptors) Len() int      { return len(r) }
func (r BackupFileDescriptors) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r BackupFileDescriptors) Less(i, j int) bool {
	if cmp := bytes.Compare(r[i].Span.Key, r[j].Span.Key); cmp != 0 {
		return cmp < 0
	}
	return bytes.Compare(r[i].Span.EndKey, r[j].Span.EndKey) < 0
}

// ReadBackupDescriptorFromURI creates an export store from the given URI, then
// reads and unmarshals a BackupDescriptor at the standard location in the
// export storage.
func ReadBackupDescriptorFromURI(
	ctx context.Context,
	uri string,
	makeExternalStorageFromURI cloud.ExternalStorageFromURIFactory,
	encryption *roachpb.FileEncryptionOptions,
) (BackupDescriptor, error) {
	exportStore, err := makeExternalStorageFromURI(ctx, uri)

	if err != nil {
		return BackupDescriptor{}, err
	}
	defer exportStore.Close()
	backupDesc, err := readBackupDescriptor(ctx, exportStore, BackupDescriptorName, encryption)
	if err != nil {
		backupManifest, manifestErr := readBackupDescriptor(ctx, exportStore, BackupManifestName, encryption)
		if manifestErr != nil {
			return BackupDescriptor{}, err
		}
		backupDesc = backupManifest
	}
	backupDesc.Dir = exportStore.Conf()
	// TODO(dan): Sanity check this BackupDescriptor: non-empty EndTime,
	// non-empty Paths, and non-overlapping Spans and keyranges in Files.
	return backupDesc, nil
}

// readBackupDescriptor reads and unmarshals a BackupDescriptor from filename in
// the provided export store.
func readBackupDescriptor(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *roachpb.FileEncryptionOptions,
) (BackupDescriptor, error) {
	r, err := exportStore.ReadFile(ctx, filename)
	if err != nil {
		return BackupDescriptor{}, err
	}
	defer r.Close()
	descBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return BackupDescriptor{}, err
	}
	if encryption != nil {
		descBytes, err = storageccl.DecryptFile(descBytes, encryption.Key)
		if err != nil {
			return BackupDescriptor{}, err
		}
	}
	var backupDesc BackupDescriptor
	if err := protoutil.Unmarshal(descBytes, &backupDesc); err != nil {
		if encryption == nil && storageccl.AppearsEncrypted(descBytes) {
			return BackupDescriptor{}, errors.Wrapf(
				err, "file appears encrypted -- try specifying %q", backupOptEncPassphrase)
		}
		return BackupDescriptor{}, err
	}
	for _, d := range backupDesc.Descriptors {
		// Calls to GetTable are generally frowned upon.
		// This specific call exists to provide backwards compatibility with
		// backups created prior to version 19.1. Starting in v19.1 the
		// ModificationTime is always written in backups for all versions
		// of table descriptors. In earlier cockroach versions only later
		// table descriptor versions contain a non-empty ModificationTime.
		// Later versions of CockroachDB use the MVCC timestamp to fill in
		// the ModificationTime for table descriptors. When performing a restore
		// we no longer have access to that MVCC timestamp but we can set it
		// to a value we know will be safe.
		if t := d.GetTable(); t == nil {
			continue
		} else if t.Version == 1 && t.ModificationTime.IsEmpty() {
			t.ModificationTime = hlc.Timestamp{WallTime: 1}
		}
	}
	return backupDesc, err
}

func readBackupPartitionDescriptor(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *roachpb.FileEncryptionOptions,
) (BackupPartitionDescriptor, error) {
	r, err := exportStore.ReadFile(ctx, filename)
	if err != nil {
		return BackupPartitionDescriptor{}, err
	}
	defer r.Close()
	descBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return BackupPartitionDescriptor{}, err
	}
	if encryption != nil {
		descBytes, err = storageccl.DecryptFile(descBytes, encryption.Key)
		if err != nil {
			return BackupPartitionDescriptor{}, err
		}
	}
	var backupDesc BackupPartitionDescriptor
	if err := protoutil.Unmarshal(descBytes, &backupDesc); err != nil {
		return BackupPartitionDescriptor{}, err
	}
	return backupDesc, err
}

func writeBackupDescriptor(
	ctx context.Context,
	settings *cluster.Settings,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *roachpb.FileEncryptionOptions,
	desc *BackupDescriptor,
) error {
	sort.Sort(BackupFileDescriptors(desc.Files))

	descBuf, err := protoutil.Marshal(desc)
	if err != nil {
		return err
	}
	if encryption != nil {
		descBuf, err = storageccl.EncryptFile(descBuf, encryption.Key)
		if err != nil {
			return err
		}
	}
	return exportStore.WriteFile(ctx, filename, bytes.NewReader(descBuf))
}

// writeBackupPartitionDescriptor writes metadata (containing a locality KV and
// partial file listing) for a partitioned BACKUP to one of the stores in the
// backup.
func writeBackupPartitionDescriptor(
	ctx context.Context,
	exportStore cloud.ExternalStorage,
	filename string,
	encryption *roachpb.FileEncryptionOptions,
	desc *BackupPartitionDescriptor,
) error {
	descBuf, err := protoutil.Marshal(desc)
	if err != nil {
		return err
	}
	if encryption != nil {
		descBuf, err = storageccl.EncryptFile(descBuf, encryption.Key)
		if err != nil {
			return err
		}
	}

	return exportStore.WriteFile(ctx, filename, bytes.NewReader(descBuf))
}

func loadBackupDescs(
	ctx context.Context,
	uris []string,
	makeExternalStorageFromURI cloud.ExternalStorageFromURIFactory,
	encryption *roachpb.FileEncryptionOptions,
) ([]BackupDescriptor, error) {
	backupDescs := make([]BackupDescriptor, len(uris))

	for i, uri := range uris {
		desc, err := ReadBackupDescriptorFromURI(ctx, uri, makeExternalStorageFromURI, encryption)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read backup descriptor")
		}
		backupDescs[i] = desc
	}
	if len(backupDescs) == 0 {
		return nil, errors.Newf("no backups found")
	}
	return backupDescs, nil
}

// getBackupLocalityInfo takes a list of store URIs that together contain a
// partitioned backup, the first of which must contain the main BACKUP manifest,
// and searches for BACKUP_PART files in each store to build a map of (non-
// default) original backup locality values to URIs that currently contain
// the backup files.
func getBackupLocalityInfo(
	ctx context.Context,
	uris []string,
	p sql.PlanHookState,
	encryption *roachpb.FileEncryptionOptions,
) (jobspb.RestoreDetails_BackupLocalityInfo, error) {
	var info jobspb.RestoreDetails_BackupLocalityInfo
	if len(uris) == 1 {
		return info, nil
	}
	stores := make([]cloud.ExternalStorage, len(uris))
	for i, uri := range uris {
		conf, err := cloud.ExternalStorageConfFromURI(uri)
		if err != nil {
			return info, errors.Wrapf(err, "export configuration")
		}
		store, err := p.ExecCfg().DistSQLSrv.ExternalStorage(ctx, conf)
		if err != nil {
			return info, errors.Wrapf(err, "make storage")
		}
		defer store.Close()
		stores[i] = store
	}

	// First read the main backup descriptor, which is required to be at the first
	// URI in the list. We don't read the table descriptors, so there's no need to
	// upgrade them.
	mainBackupDesc, err := readBackupDescriptor(ctx, stores[0], BackupDescriptorName, encryption)
	if err != nil {
		manifest, manifestErr := readBackupDescriptor(ctx, stores[0], BackupManifestName, encryption)
		if manifestErr != nil {
			return info, err
		}
		mainBackupDesc = manifest
	}

	// Now get the list of expected partial per-store backup manifest filenames
	// and attempt to find them.
	urisByOrigLocality := make(map[string]string)
	for _, filename := range mainBackupDesc.PartitionDescriptorFilenames {
		found := false
		for i, store := range stores {
			if desc, err := readBackupPartitionDescriptor(ctx, store, filename, encryption); err == nil {
				if desc.BackupID != mainBackupDesc.ID {
					return info, errors.Errorf(
						"expected backup part to have backup ID %s, found %s",
						mainBackupDesc.ID, desc.BackupID,
					)
				}
				origLocalityKV := desc.LocalityKV
				kv := roachpb.Tier{}
				if err := kv.FromString(origLocalityKV); err != nil {
					return info, errors.Wrapf(err, "reading backup manifest from %s", uris[i])
				}
				if _, ok := urisByOrigLocality[origLocalityKV]; ok {
					return info, errors.Errorf("duplicate locality %s found in backup", origLocalityKV)
				}
				urisByOrigLocality[origLocalityKV] = uris[i]
				found = true
				break
			}
		}
		if !found {
			return info, errors.Errorf("expected manifest %s not found in backup locations", filename)
		}
	}
	info.URIsByOriginalLocalityKV = urisByOrigLocality
	return info, nil
}

func loadSQLDescsFromBackupsAtTime(
	backupDescs []BackupDescriptor, asOf hlc.Timestamp,
) ([]sqlbase.Descriptor, BackupDescriptor) {
	lastBackupDesc := backupDescs[len(backupDescs)-1]

	if asOf.IsEmpty() {
		return lastBackupDesc.Descriptors, lastBackupDesc
	}

	for _, b := range backupDescs {
		if asOf.Less(b.StartTime) {
			break
		}
		lastBackupDesc = b
	}
	if len(lastBackupDesc.DescriptorChanges) == 0 {
		return lastBackupDesc.Descriptors, lastBackupDesc
	}

	byID := make(map[sqlbase.ID]*sqlbase.Descriptor, len(lastBackupDesc.Descriptors))
	for _, rev := range lastBackupDesc.DescriptorChanges {
		if asOf.Less(rev.Time) {
			break
		}
		if rev.Desc == nil {
			delete(byID, rev.ID)
		} else {
			byID[rev.ID] = rev.Desc
		}
	}

	allDescs := make([]sqlbase.Descriptor, 0, len(byID))
	for _, desc := range byID {
		if t := desc.Table(hlc.Timestamp{}); t != nil {
			// A table revisions may have been captured before it was in a DB that is
			// backed up -- if the DB is missing, filter the table.
			if byID[t.ParentID] == nil {
				continue
			}
		}
		allDescs = append(allDescs, *desc)
	}
	return allDescs, lastBackupDesc
}

// sanitizeLocalityKV returns a sanitized version of the input string where all
// characters that are not alphanumeric or -, =, or _ are replaced with _.
func sanitizeLocalityKV(kv string) string {
	sanitizedKV := make([]byte, len(kv))
	for i := 0; i < len(kv); i++ {
		if (kv[i] >= 'a' && kv[i] <= 'z') ||
			(kv[i] >= 'A' && kv[i] <= 'Z') ||
			(kv[i] >= '0' && kv[i] <= '9') || kv[i] == '-' || kv[i] == '=' {
			sanitizedKV[i] = kv[i]
		} else {
			sanitizedKV[i] = '_'
		}
	}
	return string(sanitizedKV)
}

func readEncryptionOptions(
	ctx context.Context, src cloud.ExternalStorage,
) (*EncryptionInfo, error) {
	r, err := src.ReadFile(ctx, "encryption-info")
	if err != nil {
		return nil, errors.Wrap(err, "could not find or read encryption information")
	}
	defer r.Close()
	encInfoBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(err, "could not find or read encryption information")
	}
	var encInfo EncryptionInfo
	if err := protoutil.Unmarshal(encInfoBytes, &encInfo); err != nil {
		return nil, err
	}
	return &encInfo, nil
}

func writeEncryptionOptions(
	ctx context.Context, opts *EncryptionInfo, dest cloud.ExternalStorage,
) error {
	buf, err := protoutil.Marshal(opts)
	if err != nil {
		return err
	}
	if err := dest.WriteFile(ctx, "encryption-info", bytes.NewReader(buf)); err != nil {
		return err
	}
	return nil
}

// VerifyUsableExportTarget ensures that the target location does not already
// contain a BACKUP or checkpoint and writes an empty checkpoint, both verifying
// that the location is writable and locking out accidental concurrent
// operations on that location if subsequently try this check. Callers must
// clean up the written checkpoint file (BackupDescriptorCheckpointName) only
// after writing to the backup file location (BackupDescriptorName).
func VerifyUsableExportTarget(
	ctx context.Context,
	settings *cluster.Settings,
	exportStore cloud.ExternalStorage,
	readable string,
	encryption *roachpb.FileEncryptionOptions,
) error {
	if r, err := exportStore.ReadFile(ctx, BackupDescriptorName); err == nil {
		// TODO(dt): If we audit exactly what not-exists error each ExternalStorage
		// returns (and then wrap/tag them), we could narrow this check.
		r.Close()
		return pgerror.Newf(pgcode.FileAlreadyExists,
			"%s already contains a %s file",
			readable, BackupDescriptorName)
	}
	if r, err := exportStore.ReadFile(ctx, BackupManifestName); err == nil {
		// TODO(dt): If we audit exactly what not-exists error each ExternalStorage
		// returns (and then wrap/tag them), we could narrow this check.
		r.Close()
		return pgerror.Newf(pgcode.FileAlreadyExists,
			"%s already contains a %s file",
			readable, BackupManifestName)
	}
	if r, err := exportStore.ReadFile(ctx, BackupDescriptorCheckpointName); err == nil {
		r.Close()
		return pgerror.Newf(pgcode.FileAlreadyExists,
			"%s already contains a %s file (is another operation already in progress?)",
			readable, BackupDescriptorCheckpointName)
	}
	if err := writeBackupDescriptor(
		ctx, settings, exportStore, BackupDescriptorCheckpointName, encryption, &BackupDescriptor{},
	); err != nil {
		return errors.Wrapf(err, "cannot write to %s", readable)
	}
	return nil
}
