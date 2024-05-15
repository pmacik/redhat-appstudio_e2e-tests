package journey

import "fmt"
import "time"

import logging "github.com/redhat-appstudio/e2e-tests/tests/load-tests/pkg/logging"

import framework "github.com/redhat-appstudio/e2e-tests/pkg/framework"


func purgeStage(f *framework.Framework, namespace string) error {
	var err error

	err = f.AsKubeDeveloper.HasController.DeleteAllApplicationsInASpecificNamespace(namespace, time.Minute * 5)
	if err != nil {
		return fmt.Errorf("Error when deleting resources in namespace %s: %v", namespace, err)
	}

	err = f.AsKubeDeveloper.HasController.DeleteAllComponentDetectionQueriesInASpecificNamespace(namespace, time.Minute * 5)
	if err != nil {
		return fmt.Errorf("Error when deleting component detection queries in namespace %s: %v", namespace, err)
	}

	err = DeleteAllBuildPipelineSelectors(f, namespace, time.Minute * 5)
	if err != nil {
		return fmt.Errorf("Error when deleting build pipeline selectors in namespace %s: %v", namespace, err)
	}

	logging.Logger.Debug("Finished purging namespace %s", namespace)
	return nil
}

func purgeCi(f *framework.Framework, username string) error {
	var err error

	_, err = f.SandboxController.DeleteUserSignup(username)
	if err != nil {
		return fmt.Errorf("Error when deleting user signup %s: %v", username, err)
	}

	logging.Logger.Debug("Finished purging user %s", username)
	return nil
}

func Purge() error {
	errCounter := 0

	for _, ctx := range MainContexts {
		if ctx.Opts.Stage {
			err := purgeStage(ctx.Framework, ctx.Namespace)
			if err != nil {
				logging.Logger.Error("Error when purging Stage: %v", err)
				errCounter++
			}
		} else {
			err := purgeCi(ctx.Framework, ctx.Username)
			if err != nil {
				logging.Logger.Error("Error when purging CI: %v", err)
				errCounter++
			}
		}
	}

	if errCounter > 0 {
		return fmt.Errorf("Hit %d errors when purging resources", errCounter)
	} else {
		logging.Logger.Info("No errors when purging resources")
		return nil
	}
}
