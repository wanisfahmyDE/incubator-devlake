/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"github.com/swaggo/swag"

	"github.com/apache/incubator-devlake/core/context"
	"github.com/apache/incubator-devlake/core/errors"
	"github.com/apache/incubator-devlake/core/plugin"
	"github.com/apache/incubator-devlake/impls/logruslog"
	_ "github.com/apache/incubator-devlake/server/api/docs"
	"github.com/apache/incubator-devlake/server/api/ping"
	"github.com/apache/incubator-devlake/server/api/shared"
	"github.com/apache/incubator-devlake/server/api/version"
	"github.com/apache/incubator-devlake/server/services"
)

const DB_MIGRATION_REQUIRED = `
New migration scripts detected. Database migration is required to launch DevLake.
WARNING: Performing migration may wipe collected data for consistency and re-collecting data may be required.
To proceed, please send a request to <config-ui-endpoint>/api/proceed-db-migration (or <devlake-endpoint>/proceed-db-migration).
Alternatively, you may downgrade back to the previous DevLake version.
`

var basicRes context.BasicRes

func Init() {
	// Initialize services
	services.Init()
	basicRes = services.GetBasicRes()
}

// @title  DevLake Swagger API
// @version 0.1
// @description  <h2>This is the main page of devlake api</h2>
// @license.name Apache-2.0
// @host localhost:8080
// @BasePath /
func CreateAndRunApiServer() {
	// Setup and run the server
	Init()
	router := CreateApiServer()
	SetupApiServer(router)
	RunApiServer(router)
}

func CreateApiServer() *gin.Engine {
	// Create router
	router := gin.Default()

	// For both protected and unprotected routes
	router.GET("/ping", ping.Get)
	router.GET("/health", ping.Get)
	router.GET("/version", version.Get)

	// Api keys
	router.Use(RestAuthentication(router, basicRes))
	router.Use(OAuth2ProxyAuthentication(basicRes))

	return router
}

func SetupApiServer(router *gin.Engine) {
	// Set gin mode
	gin.SetMode(basicRes.GetConfig("MODE"))

	// Endpoint to proceed database migration
	router.GET("/proceed-db-migration", func(ctx *gin.Context) {
		// Check if migration requires confirmation
		if !services.MigrationRequireConfirmation() {
			// Return success response
			shared.ApiOutputSuccess(ctx, nil, http.StatusOK)
			return
		}
		// Execute database migration
		err := services.ExecuteMigration()
		if err != nil {
			// Return error response
			shared.ApiOutputError(ctx, errors.Default.Wrap(err, "error executing migration"))
			return
		}
		// Return success response
		shared.ApiOutputSuccess(ctx, nil, http.StatusOK)
	})

	// Restrict access if database migration is required
	router.Use(func(ctx *gin.Context) {
		if !services.MigrationRequireConfirmation() {
			return
		}
		// Return error response
		shared.ApiOutputError(
			ctx,
			errors.HttpStatus(http.StatusPreconditionRequired).New(DB_MIGRATION_REQUIRED),
		)
		ctx.Abort()
	})

	// Add swagger handlers
	router.GET("/swagger/*any", modifyBasePath, ginSwagger.WrapHandler(swaggerFiles.Handler))
	registerExtraOpenApiSpecs(router)

	// Add debug logging for endpoints
	gin.DebugPrintRouteFunc = func(httpMethod, absolutePath, handlerName string, nuHandlers int) {
		logruslog.Global.Printf("endpoint %v %v %v %v", httpMethod, absolutePath, handlerName, nuHandlers)
	}

	// Enable CORS
	router.Use(cors.New(cors.Config{
		// Allow all origins
		AllowOrigins: []string{"*"},
		// Allow common methods
		AllowMethods: []string{"PUT", "PATCH", "POST", "GET", "OPTIONS"},
		// Allow common headers
		AllowHeaders: []string{"Origin", "Content-Type"},
		// Expose these headers
		ExposeHeaders: []string{"Content-Length"},
		// Allow credentials
		AllowCredentials: true,
		// Cache for 2 hours
		MaxAge: 120 * time.Hour,
	}))

	// Register API endpoints
	RegisterRouter(router, basicRes)
}

func RunApiServer(router *gin.Engine) {
	// Get port from config
	port := basicRes.GetConfig("PORT")
	// Trim any : from the start
	port = strings.TrimLeft(port, ":")
	// Convert to int
	portNum, err := strconv.Atoi(port)
	if err != nil {
		// Panic if PORT is not an int
		panic(fmt.Errorf("PORT [%s] must be int: %s", port, err.Error()))
	}

	// Start the server
	err = router.Run(fmt.Sprintf(":%d", portNum))
	if err != nil {
		panic(err)
	}
}

func registerExtraOpenApiSpecs(router *gin.Engine) {
	for name, pluginMeta := range plugin.AllPlugins() {
		if pluginOpenApiSpec, ok := pluginMeta.(plugin.PluginOpenApiSpec); ok {
			spec := &swag.Spec{
				InfoInstanceName: name,
				SwaggerTemplate:  pluginOpenApiSpec.OpenApiSpec(),
			}
			swag.Register(name, spec)
			router.GET(
				fmt.Sprintf("/plugins/swagger/%s/*any", name),
				ginSwagger.CustomWrapHandler(
					&ginSwagger.Config{
						URL:                      "doc.json",
						DocExpansion:             "list",
						InstanceName:             name,
						Title:                    fmt.Sprintf("%s API", name),
						DefaultModelsExpandDepth: 1,
						DeepLinking:              true,
						PersistAuthorization:     false,
					},
					swaggerFiles.Handler,
				),
			)
		}
	}
}

type bodyTamper struct {
	gin.ResponseWriter
}

// Write intercepts the response body and modifies the base path
func (w bodyTamper) Write(b []byte) (int, error) {
	var m map[string]interface{}
	err := json.Unmarshal(b, &m)
	if err != nil {
		return 0, err
	}
	m["basePath"] = "/api"
	b, err = json.Marshal(m)
	if err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(b)
}

func modifyBasePath(c *gin.Context) {
	if !strings.HasSuffix(c.Request.URL.Path, "swagger/doc.json") {
		return
	}
	u, _ := url.Parse(c.GetHeader("Referer"))
	if u == nil || !strings.HasPrefix(u.Path, "/api") {
		return
	}
	blw := &bodyTamper{ResponseWriter: c.Writer}
	c.Writer = blw
}
