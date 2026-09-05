package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

func main() {
	router := gin.Default()
	router.LoadHTMLGlob("templates/*")
	router.Static("/static", "./static")

	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{
			"title": "Neva Concert Hall Corporate Approvals",
		})
	})

	// Timeweb App Platform uses this endpoint to check that the app is alive.
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// A tiny HTMX interaction proving that server-rendered fragments work.
	router.POST("/demo/status", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, `<span class="status status--ok">Сервис работает</span>`)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	address := "0.0.0.0:" + port
	log.Printf("starting server on %s", address)
	if err := router.Run(address); err != nil {
		log.Fatal(err)
	}
}
