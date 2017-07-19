// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package backup

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/miolini/datacounter"
	"golang.org/x/sync/errgroup"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/helpers"
)

// ProcessSmartOptions will compute the snapshots to use
func ProcessSmartOptions(jobInfo *helpers.JobInfo) error {
	snapshots, err := helpers.GetSnapshots(context.Background(), jobInfo.VolumeName)
	if err != nil {
		return err
	}
	jobInfo.BaseSnapshot = snapshots[0]
	if jobInfo.Full {
		// TODO: Check if we already have a full backup for this snapshot in the destination(s)
		return nil
	}
	lastComparableSnapshots := make([]*helpers.SnapshotInfo, len(jobInfo.Destinations))
	lastBackup := make([]*helpers.SnapshotInfo, len(jobInfo.Destinations))
	for idx := range jobInfo.Destinations {
		destBackups, derr := getBackupsForTarget(context.Background(), jobInfo.VolumeName, jobInfo.Destinations[idx], jobInfo)
		if derr != nil {
			return derr
		}
		if len(destBackups) == 0 {
			continue
		}
		lastBackup[idx] = &destBackups[0].BaseSnapshot
		if jobInfo.Incremental {
			lastComparableSnapshots[idx] = &destBackups[0].BaseSnapshot
		}
		if jobInfo.FullIfOlderThan != -1*time.Minute {
			for _, bkp := range destBackups {
				if bkp.IncrementalSnapshot.Name == "" {
					lastComparableSnapshots[idx] = &bkp.BaseSnapshot
					break
				}
			}
		}
	}

	var lastNotEqual bool
	// Verify that all "comparable" snapshots are the same across destinations
	for i := 1; i < len(lastComparableSnapshots); i++ {
		if !lastComparableSnapshots[i-1].Equal(lastComparableSnapshots[i]) {
			return fmt.Errorf("destinations are out of sync, cannot continue with smart option")
		}

		if !lastNotEqual && !lastBackup[i-1].Equal(lastBackup[i]) {
			lastNotEqual = true
		}
	}

	// Now select the proper job options and continue
	if jobInfo.Incremental {
		if lastComparableSnapshots[0] == nil {
			return fmt.Errorf("no snapshot to increment from - try doing a full backup instead")
		}
		if lastComparableSnapshots[0].Equal(&snapshots[0]) {
			return fmt.Errorf("no new snapshot to sync")
		}
		jobInfo.IncrementalSnapshot = *lastComparableSnapshots[0]
	}

	if jobInfo.FullIfOlderThan != -1*time.Minute {
		if lastComparableSnapshots[0] == nil {
			// No previous full backup, so do one
			helpers.AppLogger.Infof("No previous full backup found, performing full backup.")
			return nil
		}
		if snapshots[0].CreationTime.Sub(lastComparableSnapshots[0].CreationTime) > jobInfo.FullIfOlderThan {
			// Been more than the allotted time, do a full backup
			helpers.AppLogger.Infof("Last Full backup was %v and is more than %v before the most recent snapshot, performing full backup.", lastComparableSnapshots[0].CreationTime, jobInfo.FullIfOlderThan)
			return nil
		}
		if lastNotEqual {
			return fmt.Errorf("want to do an incremental backup but last incremental backup at destinations do not match")
		}
		if lastBackup[0].Equal(&snapshots[0]) {
			return fmt.Errorf("no new snapshot to sync")
		}
		jobInfo.IncrementalSnapshot = *lastBackup[0]
	}
	return nil
}

func getBackupsForTarget(ctx context.Context, volume, target string, jobInfo *helpers.JobInfo) ([]*helpers.JobInfo, error) {
	// Prepare the backend client
	backend := prepareBackend(ctx, jobInfo, target, nil)

	// Get the local cache dir
	localCachePath := getCacheDir(target)

	// Sync the local cache
	safeManifests, _ := syncCache(ctx, jobInfo, localCachePath, backend)

	// Read in Manifests and display
	decodedManifests := make([]*helpers.JobInfo, 0, len(safeManifests))
	for _, manifest := range safeManifests {
		manifestPath := filepath.Join(localCachePath, manifest)
		decodedManifest, oerr := readManifest(ctx, manifestPath, jobInfo)
		if oerr != nil {
			return nil, oerr
		}
		if strings.Compare(decodedManifest.VolumeName, volume) == 0 {
			decodedManifests = append(decodedManifests, decodedManifest)
		}
	}

	sort.SliceStable(decodedManifests, func(i, j int) bool {
		return decodedManifests[i].BaseSnapshot.CreationTime.After(decodedManifests[j].BaseSnapshot.CreationTime)
	})
	return decodedManifests, nil
}

// Backup will iniate a backup with the provided configuration.
func Backup(jobInfo *helpers.JobInfo) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer os.RemoveAll(helpers.BackupTempdir)

	if jobInfo.Resume {
		if err := tryResume(ctx, jobInfo); err != nil {
			return err
		}
	}

	fileBufferSize := jobInfo.MaxFileBuffer
	if fileBufferSize == 0 {
		fileBufferSize = 1
	}

	startCh := make(chan *helpers.VolumeInfo, fileBufferSize) // Sent to ZFS command and meant to be closed when done
	stepCh := make(chan *helpers.VolumeInfo, fileBufferSize)  // Used as input to first backend, closed when final manifest is sent through

	var maniwg sync.WaitGroup
	maniwg.Add(1)

	uploadBuffer := make(chan bool, jobInfo.MaxParallelUploads)
	defer close(uploadBuffer)

	fileBuffer := make(chan bool, fileBufferSize)
	for i := 0; i < fileBufferSize; i++ {
		fileBuffer <- true
	}

	var group *errgroup.Group
	group, ctx = errgroup.WithContext(ctx)

	// Used to prevent closing the upload pipeline after the ZFS command is done
	// so we can send the manifest file up after all volumes have made it to the backends.
	go func(next chan<- *helpers.VolumeInfo) {
		defer maniwg.Done()
		for vol := range startCh {
			maniwg.Add(1)
			next <- vol
		}
	}(stepCh)

	// Start the ZFS send stream
	group.Go(func() error {
		return sendStream(ctx, jobInfo, startCh, fileBuffer)
	})

	var usedBackends []backends.Backend
	var channels []<-chan *helpers.VolumeInfo
	channels = append(channels, stepCh)

	if jobInfo.MaxFileBuffer != 0 {
		jobInfo.Destinations = append(jobInfo.Destinations, backends.DeleteBackendPrefix)
	}

	// Prepare backends and setup plumbing
	for _, destination := range jobInfo.Destinations {
		backend := prepareBackend(ctx, jobInfo, destination, uploadBuffer)
		_ = getCacheDir(destination)
		out := backend.StartUpload(ctx, channels[len(channels)-1])
		channels = append(channels, out)
		usedBackends = append(usedBackends, backend)
		group.Go(backend.Wait)
	}

	// Create and copy a copy of the manifest during the backup procedure for future retry requests
	group.Go(func() error {
		defer close(fileBuffer)
		for vol := range channels[len(channels)-1] {
			if !vol.IsManifest {
				maniwg.Done()
				helpers.AppLogger.Debugf("Volume %s has finished the entire pipeline.", vol.ObjectName)
				helpers.AppLogger.Debugf("Adding %s to the manifest volume list.", vol.ObjectName)
				jobInfo.Volumes = append(jobInfo.Volumes, vol)
				// Write a manifest file and save it locally in order to resume later
				manifestVol, err := saveManifest(ctx, jobInfo, false)
				if err != nil {
					return err
				}
				if err = manifestVol.DeleteVolume(); err != nil {
					helpers.AppLogger.Warningf("Error deleting temporary manifest file  - %v", err)
				}
			} else {
				// Manifest has been processed, we're done!
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case fileBuffer <- true:
			}
		}
		return nil
	})

	// Final Manifest Creation
	group.Go(func() error {
		maniwg.Wait() // Wait until the ZFS send command has completed and all volumes have been uploaded to all backends.
		helpers.AppLogger.Infof("All volumes dispatched in pipeline, finalizing manifest file.")

		jobInfo.EndTime = time.Now()
		manifestVol, err := saveManifest(ctx, jobInfo, true)
		if err != nil {
			return err
		}
		stepCh <- manifestVol
		close(stepCh)
		return nil
	})

	err := group.Wait() // Wait for ZFS Send to finish, Backends to finish, and Manifest files to be copied/uploaded
	if err != nil {
		return err
	}

	totalWrittenBytes := jobInfo.TotalBytesWritten()
	helpers.AppLogger.Noticef("Done.\n\tTotal ZFS Stream Bytes: %d (%s)\n\tTotal Bytes Written: %d (%s)\n\tElapsed Time: %v\n\tTotal Files Uploaded: %d", jobInfo.ZFSStreamBytes, humanize.IBytes(jobInfo.ZFSStreamBytes), totalWrittenBytes, humanize.IBytes(totalWrittenBytes), time.Since(jobInfo.StartTime), len(jobInfo.Volumes)+1)

	helpers.AppLogger.Debugf("Cleaning up resources...")

	for _, backend := range usedBackends {
		if err = backend.Close(); err != nil {
			helpers.AppLogger.Warningf("Could not properly close backend due to error - %v", err)
		}
	}

	return nil
}

func saveManifest(ctx context.Context, j *helpers.JobInfo, final bool) (*helpers.VolumeInfo, error) {
	sort.Sort(helpers.ByVolumeNumber(j.Volumes))

	// Setup Manifest File
	manifest, err := helpers.CreateManifestVolume(ctx, j)
	if err != nil {
		helpers.AppLogger.Errorf("Error trying to create manifest volume - %v", err)
		return nil, err
	}
	safeManifestFile := fmt.Sprintf("%x", md5.Sum([]byte(manifest.ObjectName)))
	manifest.IsFinalManifest = final
	jsonEnc := json.NewEncoder(manifest)
	err = jsonEnc.Encode(j)
	if err != nil {
		helpers.AppLogger.Errorf("Could not JSON Encode job information due to error - %v", err)
		return nil, err
	}
	if err = manifest.Close(); err != nil {
		helpers.AppLogger.Errorf("Could not close manifest volume due to error - %v", err)
		return nil, err
	}
	for _, destination := range j.Destinations {
		if destination == backends.DeleteBackendPrefix {
			continue
		}
		safeFolder := fmt.Sprintf("%x", md5.Sum([]byte(destination)))
		dest := filepath.Join(helpers.WorkingDir, "cache", safeFolder, safeManifestFile)
		if err = manifest.CopyTo(dest); err != nil {
			helpers.AppLogger.Warningf("Could not write manifest volume due to error - %v", err)
			return nil, err
		}
		helpers.AppLogger.Debugf("Copied manifest to local cache for destination %s.", destination)
	}
	return manifest, nil
}

func sendStream(ctx context.Context, j *helpers.JobInfo, c chan<- *helpers.VolumeInfo, buffer <-chan bool) error {
	var group *errgroup.Group
	group, ctx = errgroup.WithContext(ctx)

	cmd := helpers.GetZFSSendCommand(ctx, j)
	cin, cout := io.Pipe()
	cmd.Stdout = cout
	cmd.Stderr = os.Stderr
	counter := datacounter.NewReaderCounter(cin)
	usingPipe := false
	if j.MaxFileBuffer == 0 {
		usingPipe = true
	}

	group.Go(func() error {
		var lastTotalBytes uint64
		defer close(c)
		var err error
		var volume *helpers.VolumeInfo
		skipBytes, volNum := j.TotalBytesStreamedAndVols()
		lastTotalBytes = skipBytes
		for {
			// Skipy byes if we are resuming
			if skipBytes > 0 {
				helpers.AppLogger.Debugf("Want to skip %d bytes.", skipBytes)
				written, serr := io.CopyN(ioutil.Discard, counter, int64(skipBytes))
				if serr != nil && serr != io.EOF {
					helpers.AppLogger.Errorf("Error while trying to read from the zfs stream to skip %d bytes - %v", skipBytes, serr)
					return serr
				}
				skipBytes -= uint64(written)
				helpers.AppLogger.Debugf("Skipped %d bytes of the ZFS send stream.", written)
				continue
			}

			// Setup next Volume
			if volume == nil || volume.Counter() >= (j.VolumeSize*humanize.MiByte)-50*humanize.KiByte {
				if volume != nil {
					helpers.AppLogger.Debugf("Finished creating volume %s", volume.ObjectName)
					volume.ZFSStreamBytes = counter.Count() - lastTotalBytes
					lastTotalBytes = counter.Count()
					if err = volume.Close(); err != nil {
						helpers.AppLogger.Errorf("Error while trying to close volume %s - %v", volume.ObjectName, err)
						return err
					}
					if !usingPipe {
						c <- volume
					}
				}
				<-buffer
				volume, err = helpers.CreateBackupVolume(ctx, j, volNum)
				if err != nil {
					helpers.AppLogger.Errorf("Error while creating volume %d - %v", volNum, err)
					return err
				}
				helpers.AppLogger.Debugf("Starting volume %s", volume.ObjectName)
				volNum++
				if usingPipe {
					c <- volume
				}
			}

			// Write a little at a time and break the output between volumes as needed
			_, ierr := io.CopyN(volume, counter, helpers.BufferSize*2)
			if ierr == io.EOF {
				// We are done!
				helpers.AppLogger.Debugf("Finished creating volume %s", volume.ObjectName)
				volume.ZFSStreamBytes = counter.Count() - lastTotalBytes
				lastTotalBytes = counter.Count()
				if err = volume.Close(); err != nil {
					helpers.AppLogger.Errorf("Error while trying to close volume %s - %v", volume.ObjectName, err)
					return err
				}
				if !usingPipe {
					c <- volume
				}
				return nil
			} else if ierr != nil {
				helpers.AppLogger.Errorf("Error while trying to read from the zfs stream for volume %s - %v", volume.ObjectName, ierr)
				return ierr
			}
		}
	})

	// Start the zfs send command
	helpers.AppLogger.Infof("Starting zfs send command: %s", strings.Join(cmd.Args, " "))
	err := cmd.Start()
	if err != nil {
		helpers.AppLogger.Errorf("Error starting zfs command - %v", err)
		return err
	}

	group.Go(func() error {
		defer cout.Close()
		return cmd.Wait()
	})

	defer func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			err = cmd.Process.Kill()
			if err != nil {
				helpers.AppLogger.Errorf("Could not kill zfs send command due to error - %v", err)
				return
			}
			err = cmd.Process.Release()
			if err != nil {
				helpers.AppLogger.Errorf("Could not release resources from zfs send command due to error - %v", err)
				return
			}
		}
	}()

	j.ZFSCommandLine = strings.Join(cmd.Args, " ")
	// Wait for the command to finish

	err = group.Wait()
	if err != nil {
		helpers.AppLogger.Errorf("Error waiting for zfs command to finish - %v", err)
		return err
	}
	helpers.AppLogger.Infof("zfs send completed without error")
	j.ZFSStreamBytes = counter.Count()
	return nil
}

func tryResume(ctx context.Context, j *helpers.JobInfo) error {
	// Temproary Final Manifest File
	manifest, merr := helpers.CreateManifestVolume(ctx, j)
	if merr != nil {
		helpers.AppLogger.Errorf("Error trying to create manifest volume - %v", merr)
		return merr
	}
	defer manifest.DeleteVolume()
	defer manifest.Close()

	safeManifestFile := fmt.Sprintf("%x", md5.Sum([]byte(manifest.ObjectName)))

	destination := j.Destinations[0]
	safeFolder := fmt.Sprintf("%x", md5.Sum([]byte(destination)))
	origManiPath := filepath.Join(helpers.WorkingDir, "cache", safeFolder, safeManifestFile)

	if originalManifest, oerr := readManifest(ctx, origManiPath, j); os.IsNotExist(oerr) {
		helpers.AppLogger.Info("No previous manifest file exists, nothing to resume")
	} else if oerr != nil {
		helpers.AppLogger.Errorf("Could not open previous manifest file %s due to error: %v", origManiPath, oerr)
		return oerr
	} else {
		if originalManifest.Compressor != j.Compressor {
			helpers.AppLogger.Errorf("Cannot resume backup, original compressor %s != compressor specified %s", originalManifest.Compressor, j.Compressor)
			return fmt.Errorf("option mismatch")
		}

		if originalManifest.EncryptTo != j.EncryptTo {
			helpers.AppLogger.Errorf("Cannot resume backup, different encryptTo flags specified (original %v != current %v)", originalManifest.EncryptTo, j.EncryptTo)
			return fmt.Errorf("option mismatch")
		}

		if originalManifest.SignFrom != j.SignFrom {
			helpers.AppLogger.Errorf("Cannot resume backup, different signFrom flags specified (original %v != current %v)", originalManifest.SignFrom, j.SignFrom)
			return fmt.Errorf("option mismatch")
		}

		currentCMD := helpers.GetZFSSendCommand(ctx, j)
		oldCMD := helpers.GetZFSSendCommand(ctx, originalManifest)
		oldCMDLine := strings.Join(currentCMD.Args, " ")
		currentCMDLine := strings.Join(oldCMD.Args, " ")
		if strings.Compare(oldCMDLine, currentCMDLine) != 0 {
			helpers.AppLogger.Errorf("Cannot resume backup, different options given for zfs send command: `%s` != current `%s`", oldCMDLine, currentCMDLine)
			return fmt.Errorf("option mismatch")
		}

		j.Volumes = originalManifest.Volumes
		j.StartTime = originalManifest.StartTime
		helpers.AppLogger.Infof("Will be resuming previous backup attempt.")
	}
	return nil
}
