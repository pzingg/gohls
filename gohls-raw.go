/*

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU General Public License as published by
    the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU General Public License for more details.

    You should have received a copy of the GNU General Public License
    along with this program.  If not, see <http://www.gnu.org/licenses/>.

*/

package main

import (
	"encoding/binary"
  "flag"
  "fmt"
  "io"
  "net/http"
  "net/url"
  "log"
  "os"
	"os/exec"
  "strings"
  "time"
  "github.com/golang/groupcache/lru"
  "github.com/kz26/m3u8"
	"code.google.com/p/portaudio-go/portaudio"
)

const VERSION = "1.0.5"

const sampleRate float64 = 44100
const framesPerBuffer int = 8192
const numChannels int = 2

var USER_AGENT string
var RAW_FORMAT string

var client = &http.Client{}

func doRequest(c *http.Client, req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", USER_AGENT)
	resp, err := c.Do(req)
	return resp, err
}

type Download struct {
	URI string
	totalDuration time.Duration
}

func downloadSegment(out io.WriteCloser, dlc chan *Download, recTime time.Duration) {
	for v := range dlc {
		req, err := http.NewRequest("GET", v.URI, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := doRequest(client, req)
		if err != nil {
			log.Print(err)
			continue
		}
		if resp.StatusCode != 200 {
			log.Printf("Received HTTP %v for %v\n", resp.StatusCode, v.URI)
			continue
		}
		// pipe output to ffmpeg here
		_, err = io.Copy(out, resp.Body)
		if err != nil {
			log.Fatal(err)
		}
		resp.Body.Close()
		log.Printf("Downloaded %v\n", v.URI)
		if recTime != 0 {
			log.Printf("Recorded %v of %v\n", v.totalDuration, recTime)
			} else {
				log.Printf("Recorded %v\n", v.totalDuration)
			}
	}
}

func getPlaylist(urlStr string, recTime time.Duration, useLocalTime bool, dlc chan *Download) {
	startTime := time.Now()
	var recDuration time.Duration = 0
	cache := lru.New(1024)
	playlistUrl, err := url.Parse(urlStr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		req, err := http.NewRequest("GET", urlStr, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := doRequest(client, req)
		if err != nil {
			log.Print(err)
			time.Sleep(time.Duration(3) * time.Second)
		}
		playlist, listType, err := m3u8.DecodeFrom(resp.Body, true)
		if err != nil {
			log.Fatal(err)
		}
		resp.Body.Close()
		if listType == m3u8.MEDIA {
			mpl := playlist.(*m3u8.MediaPlaylist)
			for _, v := range mpl.Segments {
				if v != nil {
					var msURI string
					if strings.HasPrefix(v.URI, "http") {
						msURI, err = url.QueryUnescape(v.URI)
						if err != nil {
							log.Fatal(err)
						}
					} else {
						msUrl, err := playlistUrl.Parse(v.URI)
						if err != nil {
							log.Print(err)
							continue
						}
						msURI, err = url.QueryUnescape(msUrl.String())
						if err != nil {
							log.Fatal(err)
						}
					}
					_, hit := cache.Get(msURI)
					if !hit {
						cache.Add(msURI, nil)
						if useLocalTime {
							recDuration = time.Now().Sub(startTime)
						} else {
							recDuration += time.Duration(int64(v.Duration * 1000000000))
						}
						dlc <- &Download{msURI, recDuration}
					}
					if recTime != 0 && recDuration != 0 && recDuration >= recTime {
						close(dlc)
						return
					}
				}
			}
			if mpl.Closed {
					close(dlc)
					return
			} else {
				time.Sleep(time.Duration(int64(mpl.TargetDuration * 1000000000)))
			}
		} else {
			log.Fatal("Not a valid media playlist")
		}
	}
}

type AudioPlayer struct {
	*portaudio.Stream
	Source io.Reader
}

func (g *AudioPlayer) fetchPcm16(out []int16) {
	var err error
	for i := range out {
		err = binary.Read(g.Source, binary.LittleEndian, out[i:i+1])
		chk(err)
	}
}

func (g *AudioPlayer) fetchPcm32(out []int32) {
	var err error
	for i := range out {
		err = binary.Read(g.Source, binary.LittleEndian, out[i:i+1])
		chk(err)
	}
}

func newAudioPlayer(source io.Reader, format string) *AudioPlayer {
	var err error
	s := &AudioPlayer{nil, source}
	if format == "s16le" {
		s.Stream, err = portaudio.OpenDefaultStream(0, numChannels, sampleRate, framesPerBuffer, s.fetchPcm16)
	} else {
		s.Stream, err = portaudio.OpenDefaultStream(0, numChannels, sampleRate, framesPerBuffer, s.fetchPcm32)
	}
	chk(err)
	return s
}

func main() {

	flag.StringVar(&RAW_FORMAT, "f", "s16le", "RAW format (s16le or s32le)")
	duration := flag.Duration("t", time.Duration(0), "Recording duration (0 == infinite)")
	useLocalTime := flag.Bool("l", false, "Use local time to track duration instead of supplied metadata")
	flag.StringVar(&USER_AGENT, "ua", fmt.Sprintf("gohls/%v", VERSION), "User-Agent for HTTP client")
	flag.Parse()

	os.Stderr.Write([]byte(fmt.Sprintf("gohls %v - HTTP Live Streaming (HLS) downloader\n", VERSION)))
	os.Stderr.Write([]byte("Copyright (C) 2013-2014 Kevin Zhang. Licensed for use under the GNU GPL version 3.\n"))

	validArgs := true
	errorMsg := ""
	if !(RAW_FORMAT == "s16le" || RAW_FORMAT == "s32le") {
		validArgs = false
		errorMsg = "Format must be s16le or s32le"
	}
	if !strings.HasPrefix(flag.Arg(0), "http") {
		validArgs = false
		errorMsg = "Media playlist url must begin with http/https"
	}
	if flag.NArg() < 1 {
		validArgs = false
	}
	if !validArgs {
		os.Stderr.Write([]byte("Usage: gohls [-f format] [-l=bool] [-t duration] [-ua user-agent] media-playlist-url\n"))
		flag.PrintDefaults()
		if errorMsg != "" {
			os.Stderr.Write([]byte(errorMsg + "\n"))
		}
		os.Exit(2)
	}

	ffmpeg := exec.Command("/usr/local/bin/ffmpeg", 
		"-i",		"pipe:0", 
		"-f",		RAW_FORMAT, 
		"-c:a", "pcm_" + RAW_FORMAT,
		"-ab",  "128000",
		"-ar",  "44100",
		"-ac",	"2",
		"-channel_layout", "stereo",
		"-")
		
	sink, err := ffmpeg.StdinPipe()
	chk(err)
	
	src, err := ffmpeg.StdoutPipe()
	chk(err)
	
	err = ffmpeg.Start()
	chk(err)
	
	portaudio.Initialize()
	defer portaudio.Terminate()

	s := newAudioPlayer(src, RAW_FORMAT)
	defer s.Close()
	
	err = s.Start()
	chk(err)
	
	defer s.Stop()
	
	msChan := make(chan *Download, 1024)
	go getPlaylist(flag.Arg(0), *duration, *useLocalTime, msChan)
	
	downloadSegment(sink, msChan, *duration)
	
}

func chk(err error) {
	if err != nil {
		panic(err)
	}
}
