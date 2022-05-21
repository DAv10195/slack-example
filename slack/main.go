package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/boltdb/bolt"
	"github.com/gin-gonic/gin"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

type Input struct {
	Num1 int `json:"num1"`
	Num2 int `json:"num2"`
}

type Output struct {
	Sum int `json:"sum"`
}

func configureDb() (*bolt.DB, error) {
	db, err := bolt.Open("slack.db", 0600, nil)
	if err != nil {
		return nil, err
	}
	if err = db.Update(func(tx *bolt.Tx) error {
		if _, err = tx.CreateBucketIfNotExists([]byte("tokens")); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return db, nil
}

func handleInstallation(db *bolt.DB) func(c *gin.Context) {
	return func(c *gin.Context) {
		_, errExists := c.GetQuery("error")
		if errExists {
			c.String(http.StatusOK, "error installing app")
			return
		}
		code, codeExists := c.GetQuery("code")
		if !codeExists {
			c.String(http.StatusBadRequest, "missing mandatory 'code' query parameter")
			return
		}
		resp, err := slack.GetOAuthV2Response(http.DefaultClient,
			os.Getenv("CLIENT_ID"),
			os.Getenv("CLIENT_SECRET"),
			code,
			"")
		if err != nil {
			c.String(http.StatusInternalServerError, "error exchanging temporary code for access token: %s", err.Error())
			return
		}
		if err = db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte("tokens"))
			if bucket == nil {
				return errors.New("error accessing tokens bucket")
			}
			return bucket.Put([]byte(resp.Team.ID), []byte(resp.AccessToken))
		}); err != nil {
			c.String(http.StatusInternalServerError, "error storing slack access token: %s", err.Error())
			return
		}
		c.Redirect(http.StatusFound, fmt.Sprintf("slack://app?team=%s&id=%s&tab=about", resp.Team.ID, resp.AppID))
	}
}

func signatureVerification(c *gin.Context) {
	verifier, err := slack.NewSecretsVerifier(c.Request.Header, os.Getenv("SIGNATURE_SECRET"))
	if err != nil {
		c.String(http.StatusBadRequest, "error initializing signature verifier: %s", err.Error())
		return
	}
	bodyBytes, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, "error reading request body: %s", err.Error())
		return
	}
	bodyBytesCopy := make([]byte, len(bodyBytes))
	copy(bodyBytesCopy, bodyBytes)
	c.Request.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytesCopy))
	if _, err = verifier.Write(bodyBytes); err != nil {
		c.String(http.StatusInternalServerError, "error writing request body bytes for verification: %s", err.Error())
		return
	}
	if err = verifier.Ensure(); err != nil {
		c.String(http.StatusUnauthorized, "error verifying slack signature: %s", err.Error())
		return
	}
	c.Next()
}

func handlePlus(db *bolt.DB) func(c *gin.Context) {
	return func(c *gin.Context) {
		cmd, err := slack.SlashCommandParse(c.Request)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid slash command payload: %s", err.Error())
			return
		}
		inputArr := strings.Split(strings.TrimSpace(cmd.Text), " ")
		if len(inputArr) != 2 {
			c.String(http.StatusBadRequest, "invalid number of input parameters provided")
			return
		}
		num1, err := strconv.Atoi(inputArr[0])
		if err != nil {
			c.String(http.StatusBadRequest, "invalid 1st input parameter: %s", err.Error())
			return
		}
		num2, err := strconv.Atoi(inputArr[1])
		if err != nil {
			c.String(http.StatusBadRequest, "invalid 2nd input parameter: %s", err.Error())
			return
		}
		buf, err := json.Marshal(&Input{
			Num1: num1,
			Num2: num2,
		})
		if err != nil {
			c.String(http.StatusInternalServerError, "error preparing request body: %s", err.Error())
			return
		}
		resp, err := http.Post("http://localhost:8080", "application/json", bytes.NewBuffer(buf))
		if err != nil {
			c.String(http.StatusInternalServerError, "error preparing http request: %s", err.Error())
			return
		}
		if resp.StatusCode != http.StatusOK {
			c.String(http.StatusInternalServerError, "invalid status code returned: %d", resp.StatusCode)
			return
		}
		output := &Output{}
		if err = json.NewDecoder(resp.Body).Decode(output); err != nil {
			c.String(http.StatusInternalServerError, "invalid response: %s", err.Error())
			return
		}
		var token string
		if err = db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte("tokens"))
			if bucket == nil {
				return errors.New("error accessing tokens bucket")
			}
			token = string(bucket.Get([]byte(cmd.TeamID)))
			return nil
		}); err != nil {
			c.String(http.StatusInternalServerError, "error reading slack access token: %s", err.Error())
			return
		}
		if _, _, _, err = slack.New(token).SendMessage(cmd.UserID, slack.MsgOptionText(fmt.Sprintf("Sum is %d", output.Sum), true)); err != nil {
			c.String(http.StatusInternalServerError, "error sending slack message: %s", err.Error())
			return
		}
	}
}

func handleEvent(db *bolt.DB) func(c *gin.Context) {
	return func(c *gin.Context) {
		bodyBytes, err := ioutil.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusInternalServerError, "error reading slack event payload: %s", err.Error())
			return
		}
		event, err := slackevents.ParseEvent(bodyBytes, slackevents.OptionNoVerifyToken())
		switch event.Type {
		case slackevents.URLVerification:
			ve, ok := event.Data.(*slackevents.EventsAPIURLVerificationEvent)
			if !ok {
				c.String(http.StatusBadRequest, "invalid url verification event payload sent from slack")
				return
			}
			c.JSON(http.StatusOK, &slackevents.ChallengeResponse{
				Challenge: ve.Challenge,
			})
		case slackevents.AppRateLimited:
			c.String(http.StatusOK, "ack")
		case slackevents.CallbackEvent:
			ce, ok := event.Data.(*slackevents.EventsAPICallbackEvent)
			if !ok {
				c.String(http.StatusBadRequest, "invalid callback event payload sent from slack")
				return
			}
			ie := &slackevents.EventsAPIInnerEvent{}
			if err = json.Unmarshal(*ce.InnerEvent, ie); err != nil {
				c.String(http.StatusBadRequest, "invalid inner event payload sent from slack: %s", err.Error())
				return
			}
			switch ie.Type {
			case slackevents.AppUninstalled:
				if err = db.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket([]byte("tokens"))
					if bucket == nil {
						return errors.New("error accessing tokens bucket")
					}
					return bucket.Delete([]byte(event.TeamID))
				}); err != nil {
					c.String(http.StatusInternalServerError, "error handling app uninstallation")
				}
			default:
				c.String(http.StatusBadRequest, "no handler for event of given type")
			}
		default:
			c.String(http.StatusBadRequest, "invalid event type sent from slack")
		}
	}
}

func main() {
	db, err := configureDb()
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	app := gin.Default()
	app.Any("/install", handleInstallation(db))
	cmdGroup := app.Group("/cmd")
	cmdGroup.Use(signatureVerification)
	cmdGroup.POST("/plus", handlePlus(db))
	eventGroup := app.Group("/event")
	eventGroup.Use(signatureVerification)
	eventGroup.POST("/handle", handleEvent(db))
	_ = app.Run()
}
