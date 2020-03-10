/*
Copyright 2019 GM Cruise LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"

	cfg "github.com/cruise-automation/daytona/pkg/config"
	"github.com/hashicorp/vault/api"
)

const (
	defaultKeyName           = "value"
	secretDestinationPrefix  = "DAYTONA_SECRET_DESTINATION_"
	secretStoragePathPrefix  = "VAULT_SECRET_"
	secretsStoragePathPrefix = "VAULT_SECRETS_"
)

// SecretDefinition represents is used for representing
// a secret definition input
type SecretDefinition struct {
	EnvKey            string
	SecretID          string
	SecretApex        string
	OutputDestination string

	secrets map[string]string
	plural  bool
	paths   []string
}

// SecretFetcher inspects the environment for variables that
// define secret definitions. The variables are used to guide
// the SecretFetcher in acquring and outputting the specified secrets
func SecretFetcher(client *api.Client, config cfg.Config) {
	log.Println("Starting secret fetch")

	defs := make([]*SecretDefinition, 0)

	ctx := context.Background()
	parallelReader := NewParallelReader(ctx, client.Logical(), config.Workers)

	envs := os.Environ()
	// Find where all our secret keys are in vault
	for _, env := range envs {
		// VAULT_SECRET_WHATEVER=secret/application/thing
		// VAULT_SECRETS_WHATEVER=secret/application/things
		// envKey=secretPath
		pair := strings.Split(env, "=")
		envKey := pair[0]
		apex := os.Getenv(envKey)
		if apex == "" {
			continue
		}

		def := &SecretDefinition{
			EnvKey:     envKey,
			SecretApex: apex,
			secrets:    make(map[string]string),
		}

		switch {
		case strings.HasPrefix(envKey, secretStoragePathPrefix):
			def.SecretID = strings.TrimPrefix(envKey, secretStoragePathPrefix)
			def.paths = append(def.paths, apex)
		case strings.HasPrefix(envKey, secretsStoragePathPrefix):
			def.SecretID = strings.TrimPrefix(envKey, secretsStoragePathPrefix)
			def.plural = true
		default:
			continue
		}

		// look for a corresponding secretDestinationPrefix key.
		// sometimes these can be cased inconsistently so we have to attempt normalization.
		// e.g.  VAULT_SECRET_APPLICATIONA --> DAYTONA_SECRET_DESTINATION_applicationa
		if dest := os.Getenv(secretDestinationPrefix + def.SecretID); dest != "" {
			def.OutputDestination = dest
		} else if dest := os.Getenv(secretDestinationPrefix + strings.ToLower(def.SecretID)); dest != "" {
			def.OutputDestination = dest
		} else if dest := os.Getenv(secretDestinationPrefix + strings.ToUpper(def.SecretID)); dest != "" {
			def.OutputDestination = dest
		}

		if config.SecretPayloadPath == "" && !config.SecretEnv {
			if def.OutputDestination == "" {
				log.Printf("No secret output method was configured for %s, will not attempt to retrieve secrets for this defintion", def.EnvKey)
				continue
			}
		}

		if def.plural {
			err := def.Walk(client)
			if err != nil {
				log.Fatalln(err)
			}
		}

		for _, path := range def.paths {
			parallelReader.AsyncRequestKeyPath(path)
		}
		for range def.paths {
			secretResult := parallelReader.ReadSecretResult()
			if secretResult.Err != nil {
				log.Fatalln(secretResult.Err)
			}

			err := def.addSecrets(client, secretResult)
			if err != nil {
				log.Fatalln(err)
			}
		}

		defs = append(defs, def)
	}

	// output the secret definitions
	for _, def := range defs {
		if config.SecretEnv {
			setEnvSecrets(def.secrets)
		}

		if def.OutputDestination != "" {
			writeSecretsToDestination(def)
		}

		if config.SecretPayloadPath != "" {
			err := writeJSONSecrets(def.secrets, config.SecretPayloadPath)
			if err != nil {
				log.Fatalln(err)
			}
		}
	}
}

func writeSecretsToDestination(def *SecretDefinition) error {
	if def.plural {
		err := writeJSONSecrets(def.secrets, def.OutputDestination)
		if err != nil {
			return err
		}
	} else {
		for _, secretValue := range def.secrets {
			err := ioutil.WriteFile(def.OutputDestination, []byte(secretValue), 0600)
			if err != nil {
				return fmt.Errorf("could not write secrets to file '%s': %s", def.OutputDestination, err)
			}
			log.Printf("Wrote secret to %s\n", def.OutputDestination)
		}
	}
	return nil
}

func writeJSONSecrets(secrets map[string]string, filepath string) error {
	payloadJSON, err := json.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("failed to convert secrets payload to json: %s", err)
	}
	err = ioutil.WriteFile(filepath, payloadJSON, 0600)
	if err != nil {
		return fmt.Errorf("could not write secrets to file '%s': %s", filepath, err)
	}
	log.Printf("Wrote %d secrets to %s\n", len(secrets), filepath)
	return nil
}

func setEnvSecrets(secrets map[string]string) error {
	for k, v := range secrets {
		err := os.Setenv(k, v)
		if err != nil {
			return fmt.Errorf("Error from os.Setenv: %s", err)
		}
		log.Printf("Set env var: %s\n", k)
	}
	return nil
}

func (sd *SecretDefinition) addSecrets(client *api.Client, secretResult *SecretResult) error {
	keyPath := secretResult.KeyPath
	secret := secretResult.Secret
	err := secretResult.Err

	_, keyName := path.Split(keyPath)
	if err != nil {
		log.Fatalf("Failed retrieving secret %s: %s\n", keyPath, err)
	}
	if secret == nil {
		log.Fatalf("Vault listed a secret '%s', but got not-found trying to read it at '%s'; very strange\n", keyName, keyPath)
	}
	secretData := secret.Data

	// Return last error encountered during processing, if any
	var lastErr error

	for k, v := range secretData {
		switch k {
		case defaultKeyName:
			sd.secrets[keyName] = v.(string)
		default:
			expandedKeyName := fmt.Sprintf("%s_%s", keyName, k)
			sd.secrets[expandedKeyName] = v.(string)
		}
	}
	return lastErr
}

// Walk walks a SecretDefintions SecretApex. This is used for iteration
// of the provided apex path
func (sd *SecretDefinition) Walk(client *api.Client) error {
	paths := make([]string, 0)

	list, err := client.Logical().List(sd.SecretApex)
	if err != nil {
		return fmt.Errorf("there was a problem listing %s: %s", sd.SecretApex, err)
	}
	if list == nil || len(list.Data) == 0 {
		return fmt.Errorf("no secrets found under: %s", sd.SecretApex)
	}
	log.Println("Starting iteration on", sd.SecretApex)
	// list.Data is like: map[string]interface {}{"keys":[]interface {}{"API_KEY", "APPLICATION_KEY", "DB_PASS"}}
	keys, ok := list.Data["keys"].([]interface{})
	if !ok {
		return fmt.Errorf("unexpected list.Data format: %#v", list.Data)
	}
	for _, k := range keys {
		key, ok := k.(string)
		if !ok {
			return fmt.Errorf("non-string secret name: %#v", key)
		}
		paths = append(paths, path.Join(sd.SecretApex, key))
	}
	sd.paths = paths
	return nil
}
