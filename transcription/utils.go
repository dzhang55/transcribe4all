// Package transcription implements functions for the manipulation and
// transcription of audio files.
//
// TODO(sandlerben): This package should be refactored into several more files.
package transcription

import (
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/kothar/go-backblaze.v0"
	"gopkg.in/mgo.v2"

	log "github.com/Sirupsen/logrus"
	"github.com/jordan-wright/email"
	"github.com/juju/errors"

	"github.com/dzhang55/go-torch/config"
)

// SendEmail connects to an email server at host:port and sends an email from
// address from, to address to, with subject line subject with message body.
func SendEmail(username string, password string, host string, port int, to []string, subject string, body string) error {
	auth := smtp.PlainAuth("", username, password, host)
	addr := host + ":" + strconv.Itoa(port)

	message := email.Email{
		From:    username,
		To:      to,
		Subject: subject,
		Text:    []byte(body),
	}
	if err := message.Send(addr, auth); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// ConvertAudioIntoFormat converts encoded audio into the required format.
func ConvertAudioIntoFormat(filePath, fileExt string) (string, error) {
	// http://cmusphinx.sourceforge.net/wiki/faq
	// -ar 16000 sets frequency to required 16khz
	// -ac 1 sets the number of audio channels to 1
	newPath := filePath + "." + fileExt
	os.Remove(newPath) // If it already exists, ffmpeg will throw an error
	cmd := exec.Command("ffmpeg", "-i", filePath, "-ar", "16000", "-ac", "1", newPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New(err.Error() + "\nCommand Output:" + string(out))
	}
	return newPath, nil
}

// DownloadFileFromURL locally downloads an audio file stored at url.
func DownloadFileFromURL(url string) (string, error) {
	// Taken from https://github.com/thbar/golang-playground/blob/master/download-files.go
	filePath := filePathFromURL(url)
	file, err := os.Create(filePath)
	if err != nil {
		return "", errors.Trace(err)
	}
	defer file.Close()

	// Get file contents
	response, err := http.Get(url)
	if err != nil {
		return "", errors.Trace(err)
	}
	defer response.Body.Close()

	// Write the body to file
	_, err = io.Copy(file, response.Body)
	if err != nil {
		return "", errors.Trace(err)
	}

	return filePath, nil
}

func filePathFromURL(url string) string {
	tokens := strings.Split(url, "/")
	filePath := tokens[len(tokens)-1]
	filePath = strings.Split(filePath, "?")[0]

	// ensure the filePath is unique by appending timestamp
	filePath = filePath + strconv.Itoa(int(time.Now().UnixNano()))
	return filePath
}

// SplitWavFile ensures that the input audio files to IBM are less than 100mb, with 5 seconds of redundancy between files.
func SplitWavFile(wavFilePath string) ([]string, error) {
	// http://stackoverflow.com/questions/36632511/split-audio-file-into-several-files-each-below-a-size-threshold
	// The Stack Overflow answer ultimately calculated the length of each audio chunk in seconds.
	// chunk_length_in_sec = math.ceil((duration_in_sec * file_split_size ) / wav_file_size)
	// Invariant: If ConvertAudioIntoWavFormat is called on filePath, a 95MB chunk of resulting Wav file is always 2968 seconds.
	// In the above equation, there is one constant: file_split_size = 95000000 bytes.
	// duration_in_sec is used to calculate wav_file_size, so it is canceled out in the ratio.
	// wav_file_size = (sample_rate * bit_rate * channel_count * duration_in_sec) / 8
	// sample_rate = 44100, bit_rate = 16, channels_count = 1 (stereo: 2, but Sphinx prefers 1)
	// As a chunk of the Wav file is extracted using FFMPEG, it is converted back into Flac format.
	numChunks, err := getNumChunks(wavFilePath)
	if err != nil {
		return []string{}, errors.Trace(err)
	}
	if numChunks == 1 {
		return []string{wavFilePath}, nil
	}

	chunkLengthInSeconds := 2968
	names := make([]string, numChunks)
	for i := 0; i < numChunks; i++ {
		startingSecond := i * chunkLengthInSeconds
		// 5 seconds of redundancy for each chunk after the first
		if i > 0 {
			startingSecond -= 5
		}
		newFilePath := strconv.Itoa(i) + "_" + wavFilePath
		if err := extractAudioSegment(wavFilePath, newFilePath, startingSecond, chunkLengthInSeconds); err != nil {
			return []string{}, errors.Trace(err)
		}
		names[i] = newFilePath
	}

	return names, nil
}

// getNumChunks gets file size in MB, divides by 95 MB, and add 1 more chunk in case
func getNumChunks(filePath string) (int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return -1, errors.Trace(err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return -1, errors.Trace(err)
	}

	wavFileSize := int(stat.Size())
	fileSplitSize := 95000000
	// The redundant seconds (5 seconds for every ~50 mintues) won't add own chunk
	// In case the remainder is almost the file size, add one more chunk
	numChunks := wavFileSize/fileSplitSize + 1
	return numChunks, nil
}

// extractAudioSegment uses FFMPEG to write a new audio file starting at a given time of a given length
func extractAudioSegment(inFilePath string, outFilePath string, ss int, t int) error {
	// -ss: starting second, -t: duration in seconds
	cmd := exec.Command("ffmpeg", "-i", inFilePath, "-ss", strconv.Itoa(ss), "-t", strconv.Itoa(t), outFilePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New(err.Error() + "\nOutput:\n" + string(out))
	}
	return nil
}

// MakeIBMTaskFunction returns a task function for transcription using IBM transcription functions.
// TODO(#52): Quite a lot of the transcription process could be done concurrently.
func MakeIBMTaskFunction(audioURL string, emailAddresses []string, searchWords []string) (task func(string) error, onFailure func(string, string)) {
	task = func(id string) error {
		filePath, err := DownloadFileFromURL(audioURL)
		if err != nil {
			return errors.Trace(err)
		}
		defer os.Remove(filePath)

		log.WithField("task", id).
			Debugf("Downloaded file at %s to %s", audioURL, filePath)

		wavPath, err := ConvertAudioIntoFormat(filePath, "wav")
		if err != nil {
			return errors.Trace(err)
		}
		defer os.Remove(wavPath)

		log.WithField("task", id).
			Debugf("Converted file %s to %s", filePath, wavPath)

		wavPaths, err := SplitWavFile(wavPath)
		if err != nil {
			return errors.Trace(err)
		}
		for i := 0; i < len(wavPaths); i++ {
			defer os.Remove(wavPaths[i])
		}

		log.WithField("task", id).
			Debugf("Split file %s into %d file(s)", filePath, len(wavPaths))

		ibmResults := []*IBMResult{}

		for _, wavPath := range wavPaths {
			flacPath, err := ConvertAudioIntoFormat(wavPath, "flac")
			if err != nil {
				return errors.Trace(err)
			}
			defer os.Remove(flacPath)

			log.WithField("task", id).
				Debugf("Converted file %s to %s", wavPath, flacPath)

			ibmResult, err := TranscribeWithIBM(flacPath, searchWords, config.Config.IBMUsername, config.Config.IBMPassword)
			if err != nil {
				return errors.Trace(err)
			}
			ibmResults = append(ibmResults, ibmResult)
		}
		transcription := GetTranscription(ibmResults)

		if len(config.Config.BackblazeAccountID) > 0 {
			audioURL, err := UploadFileToBackblaze(filePath, config.Config.BackblazeAccountID, config.Config.BackblazeApplicationKey, config.Config.BackblazeBucket)
			if err != nil {
				return errors.Trace(err)
			}
			transcription.AudioURL = audioURL
			log.WithField("task", id).
				Debugf("Uploaded %s to backblaze", filePath)
		}

		if len(config.Config.MongoURL) > 0 {
			if err := WriteToMongo(transcription, config.Config.MongoURL); err != nil {
				return errors.Trace(err)
			}
			log.WithField("task", id).
				Debugf("Wrote to mongo")
		}

		if len(config.Config.EmailUsername) > 0 {
			if err := SendEmail(config.Config.EmailUsername, config.Config.EmailPassword, config.Config.EmailSMTPServer, config.Config.EmailPort, emailAddresses, fmt.Sprintf("IBM Transcription %s Complete", id), "The transcript is below. It can also be found in the database."+"\n\n"+transcription.Transcript); err != nil {
				return errors.Trace(err)
			}
		}

		log.WithField("task", id).
			Debugf("Sent email to %v", emailAddresses)
		return nil
	}

	onFailure = func(id string, errMessage string) {
		err := SendEmail(config.Config.EmailUsername, config.Config.EmailPassword, "smtp.gmail.com", 587, emailAddresses, fmt.Sprintf("IBM Transcription %s Failed", id), errMessage)
		if err != nil {
			log.WithField("task", id).
				Debugf("Could not send error email to %v because of the error %v", emailAddresses, err.Error())
			return
		}
		log.WithField("task", id).
			Debugf("Sent email to %v", emailAddresses)
	}
	return task, onFailure
}

// UploadFileToBackblaze uploads the given gile to the given backblaze bucket
func UploadFileToBackblaze(filePath string, accountID string, applicationKey string, bucketName string) (string, error) {
	b2, err := backblaze.NewB2(backblaze.Credentials{
		AccountID:      accountID,
		ApplicationKey: applicationKey,
	})
	if err != nil {
		return "", errors.Trace(err)
	}

	bucket, err := b2.Bucket(bucketName)
	if err != nil {
		return "", errors.Trace(err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return "", errors.Trace(err)
	}

	name := filepath.Base(filePath)
	metadata := make(map[string]string) // empty metadata

	_, err = bucket.UploadFile(name, metadata, file)
	if err != nil {
		return "", errors.Trace(err)
	}

	url, err := bucket.FileURL(name)
	if err != nil {
		return "", errors.Trace(err)
	}
	return url, nil
}

type mgoLogger struct{}

func (mgoLogger) Output(_ int, s string) error {
	log.Debug(s)
	return nil
}

// Transcription contains the full transcription and other information.
type Transcription struct {
	Transcript  string
	AudioURL    string
	CompletedAt time.Time
	Timestamps  []timestamp
	Confidences []confidence
	Keywords    []ibmKeywordResult
}

type timestamp struct {
	Word      string
	StartTime float64
	EndTime   float64
}

type confidence struct {
	Word  string
	Score float64
}

// WriteToMongo takes a string and writes it to the database
func WriteToMongo(data *Transcription, url string) error {
	mgo.SetLogger(mgoLogger{})
	session, err := mgo.Dial(url)
	if err != nil {
		return err
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)

	c := session.DB("database").C("transcriptions")

	// Insert data
	err = c.Insert(&data)
	if err != nil {
		return err
	}

	return nil
}
