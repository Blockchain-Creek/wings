package router

import (
	"archive/zip"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/middleware"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// List versions
func getVersions(c *gin.Context) {
	// Get the folder
	var data struct {
		FOLDER string `json:"folder"`
	}

	// Validate the download url
	if err := c.BindJSON(&data); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Invalid folder."})
		return
	}

	if data.FOLDER == "" {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Invalid folder."})
		return
	}

	// List versions
	var responseFiles []string

	files, err := ioutil.ReadDir(data.FOLDER)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": err})
		return
	}

	for _, file := range files {
		responseFiles = append(responseFiles, file.Name())
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "versions": responseFiles})
}

// Switch version
func switchVersion(c *gin.Context) {
	// Get the requested server
	s := middleware.ExtractServer(c)

	// Get the folder
	var data struct {
		METHOD      string `json:"method"`
		FOLDER      string `json:"folder"`
		VERSION     string `json:"version"`
		FILENAME    string `json:"filename"`
		ACTION      string `json:"action"`
		DELETEFILES int    `json:"deleteFiles"`
	}

	// Validate the download url
	if err := c.BindJSON(&data); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Invalid folder."})
		return
	}

	// Delete files
	files, err := s.Filesystem().ListDirectory(path.Join("/"))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to load files."})
		return
	}

	for _, file := range files {
		if data.DELETEFILES == 1 {
			err := s.Filesystem().Delete(path.Join("/", file.Name()))
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to delete all files."})
				return
			}
		} else {
			splittedName := strings.Split(file.Name(), ".")
			extension := splittedName[len(splittedName)-1]

			if extension == "jar" {
				err := s.Filesystem().Delete(path.Join("/", file.Name()))
				if err != nil {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to delete all files."})
					return
				}
			}
		}
	}

	if data.METHOD == "download" {
		if data.ACTION == "unzip" {
			data.FILENAME = "version.zip"
		}

		err := DownloadVersion(path.Join(config.Get().System.Data, "/", c.Param("server"), "/", data.FILENAME), data.VERSION)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to copy new version."})
			return
		}

		if data.ACTION == "unzip" {
			_, err = UnzipVersion(path.Join(config.Get().System.Data, "/", c.Param("server"), "/", data.FILENAME), path.Join(config.Get().System.Data, "/", c.Param("server"), "/"))
			if err != nil {
				fmt.Println(err)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to unzip the version."})
				return
			}

			deleteErr := s.Filesystem().Delete(path.Join("/", data.FOLDER, "version.zip"))
			if deleteErr != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to delete the version.zip."})
				return
			}
		}
	}

	if data.METHOD == "file" {
		if data.FILENAME == "" {
			data.FILENAME = data.VERSION
		}

		_, err := copyVersion(path.Join(data.FOLDER, data.VERSION), path.Join(config.Get().System.Data, "/", c.Param("server"), "/", data.FILENAME))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Failed to copy new version."})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// Download Version
func DownloadVersion(filepath string, url string) error {
	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}

	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}

// Unzip Version
func UnzipVersion(src string, dest string) ([]string, error) {
	var filenames []string

	r, err := zip.OpenReader(src)
	if err != nil {
		return filenames, err
	}

	defer r.Close()

	for _, f := range r.File {
		// Store filename/path for returning and using later on
		fpath := filepath.Join(dest, f.Name)

		// Check for ZipSlip. More Info: http://bit.ly/2MsjAWE
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return filenames, fmt.Errorf("%s: illegal file path", fpath)
		}

		filenames = append(filenames, fpath)

		if f.FileInfo().IsDir() {
			// Make Folder
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}

		// Make File
		if err = os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return filenames, err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return filenames, err
		}

		rc, err := f.Open()
		if err != nil {
			return filenames, err
		}

		_, err = io.Copy(outFile, rc)

		// Close the file without defer to close before next iteration of loop
		outFile.Close()
		rc.Close()

		if err != nil {
			return filenames, err
		}
	}

	return filenames, nil
}

// Copy File
func copyVersion(src, dst string) (int64, error) {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer destination.Close()
	nBytes, err := io.Copy(destination, source)
	return nBytes, err
}
