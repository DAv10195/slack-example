package main

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

type Input struct {
	Num1 int `json:"num1"`
	Num2 int `json:"num2"`
}

type Output struct {
	Sum int `json:"sum"`
}

func handle(c *gin.Context) {
	input := &Input{}
	if err := json.NewDecoder(c.Request.Body).Decode(input); err != nil {
		c.String(http.StatusBadRequest, "error reading request body: %s", err.Error())
		return
	}
	c.JSON(http.StatusOK, &Output{
		Sum: input.Num1 + input.Num2,
	})
}

func main() {
	app := gin.Default()
	app.POST("/", handle)
	_ = app.Run()
}
