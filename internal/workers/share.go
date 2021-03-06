package workers

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/internal/mutex"
	"github.com/photoprism/photoprism/internal/query"
	"github.com/photoprism/photoprism/internal/remote"
	"github.com/photoprism/photoprism/internal/remote/webdav"
	"github.com/photoprism/photoprism/internal/thumb"
)

// Share represents a share worker.
type Share struct {
	conf *config.Config
}

// NewShare returns a new share worker.
func NewShare(conf *config.Config) *Share {
	return &Share{conf: conf}
}

// logError logs an error message if err is not nil.
func (worker *Share) logError(err error) {
	if err != nil {
		log.Errorf("share: %s", err.Error())
	}
}

// Start starts the share worker.
func (worker *Share) Start() (err error) {
	if err := mutex.ShareWorker.Start(); err != nil {
		event.Error(fmt.Sprintf("share: %s", err.Error()))
		return err
	}

	defer mutex.ShareWorker.Stop()

	f := form.AccountSearch{
		Share: true,
	}

	// Find accounts for which sharing is enabled
	accounts, err := query.AccountSearch(f)

	// Upload newly shared files
	for _, a := range accounts {
		if mutex.ShareWorker.Canceled() {
			return nil
		}

		if a.AccType != remote.ServiceWebDAV {
			continue
		}

		files, err := query.FileShares(a.ID, entity.FileShareNew)

		if err != nil {
			worker.logError(err)
			continue
		}

		if len(files) == 0 {
			// No files to upload for this account
			continue
		}

		client := webdav.New(a.AccURL, a.AccUser, a.AccPass)
		existingDirs := make(map[string]string)

		for _, file := range files {
			if mutex.ShareWorker.Canceled() {
				return nil
			}

			dir := filepath.Dir(file.RemoteName)

			if _, ok := existingDirs[dir]; !ok {
				if err := client.CreateDir(dir); err != nil {
					log.Errorf("share: could not create folder %s", dir)
					continue
				}
			}

			srcFileName := path.Join(worker.conf.OriginalsPath(), file.File.FileName)

			if a.ShareSize != "" {
				thumbType, ok := thumb.Types[a.ShareSize]

				if !ok {
					log.Errorf("share: invalid size %s", a.ShareSize)
					continue
				}

				srcFileName, err = thumb.FromFile(srcFileName, file.File.FileHash, worker.conf.ThumbPath(), thumbType.Width, thumbType.Height, thumbType.Options...)

				if err != nil {
					worker.logError(err)
					continue
				}
			}

			if err := client.Upload(srcFileName, file.RemoteName); err != nil {
				worker.logError(err)
				file.Errors++
				file.Error = err.Error()
			} else {
				log.Infof("share: uploaded %s to %s", file.RemoteName, a.AccName)
				file.Errors = 0
				file.Error = ""
				file.Status = entity.FileShareShared
			}

			if a.RetryLimit >= 0 && file.Errors > a.RetryLimit {
				file.Status = entity.FileShareError
			}

			if mutex.ShareWorker.Canceled() {
				return nil
			}

			worker.logError(entity.Db().Save(&file).Error)
		}
	}

	// Remove previously shared files if expired
	for _, a := range accounts {
		if mutex.ShareWorker.Canceled() {
			return nil
		}

		if a.AccType != remote.ServiceWebDAV {
			continue
		}

		files, err := query.ExpiredFileShares(a)

		if err != nil {
			worker.logError(err)
			continue
		}

		if len(files) == 0 {
			// No files to remove for this account
			continue
		}

		client := webdav.New(a.AccURL, a.AccUser, a.AccPass)

		for _, file := range files {
			if mutex.ShareWorker.Canceled() {
				return nil
			}

			if err := client.Delete(file.RemoteName); err != nil {
				file.Errors++
				file.Error = err.Error()
			} else {
				log.Infof("share: removed %s from %s", file.RemoteName, a.AccName)
				file.Errors = 0
				file.Error = ""
				file.Status = entity.FileShareRemoved
			}

			if err := entity.Db().Save(&file).Error; err != nil {
				worker.logError(err)
			}
		}
	}

	return err
}
