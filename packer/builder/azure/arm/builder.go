// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License. See the LICENSE file in the project root for license information.

package arm

import (
	"errors"
	"fmt"
	"log"

	packerAzureCommon "github.com/Azure/packer-azure/packer/builder/azure/common"

	"github.com/Azure/packer-azure/packer/builder/azure/common/constants"
	"github.com/Azure/packer-azure/packer/builder/azure/common/lin"

	"github.com/Azure/go-autorest/autorest/azure"

	"github.com/mitchellh/multistep"
	"github.com/mitchellh/packer/common"
	"github.com/mitchellh/packer/helper/communicator"
	"github.com/mitchellh/packer/packer"
)

type Builder struct {
	config   *Config
	stateBag multistep.StateBag
	runner   multistep.Runner
}

const (
	DefaultPublicIPAddressName = "packerPublicIP"
	DefaultSecretName          = "packerKeyVaultSecret"
)

func (b *Builder) Prepare(raws ...interface{}) ([]string, error) {
	c, warnings, errs := newConfig(raws...)
	if errs != nil {
		return warnings, errs
	}

	b.config = c

	b.stateBag = new(multistep.BasicStateBag)
	err := b.configureStateBag(b.stateBag)
	if err != nil {
		return nil, err
	}

	return warnings, errs
}

func (b *Builder) Run(ui packer.Ui, hook packer.Hook, cache packer.Cache) (packer.Artifact, error) {
	ui.Say("Preparing builder ...")

	b.stateBag.Put("hook", hook)
	b.stateBag.Put(constants.Ui, ui)

	spnCloud, spnKeyVault, err := b.getServicePrincipalTokens(ui.Say)
	if err != nil {
		return nil, err
	}

	ui.Message("Creating Azure Resource Manager (ARM) client ...")
	azureClient, err := NewAzureClient(
		b.config.SubscriptionID,
		b.config.ResourceGroupName,
		b.config.StorageAccount,
		spnCloud,
		spnKeyVault)

	if err != nil {
		return nil, err
	}

	var steps []multistep.Step

	if b.config.OSType == constants.Target_Linux {
		steps = []multistep.Step{
			NewStepCreateResourceGroup(azureClient, ui),
			NewStepValidateTemplate(azureClient, ui, Linux),
			NewStepDeployTemplate(azureClient, ui, Linux),
			NewStepGetIPAddress(azureClient, ui),
			&communicator.StepConnectSSH{
				Config:    &b.config.Comm,
				Host:      lin.SSHHost,
				SSHConfig: lin.SSHConfig(b.config.UserName),
			},
			&common.StepProvision{},
			NewStepGetOSDisk(azureClient, ui),
			NewStepPowerOffCompute(azureClient, ui),
			NewStepCaptureImage(azureClient, ui),
			NewStepDeleteResourceGroup(azureClient, ui),
			NewStepDeleteOSDisk(azureClient, ui),
		}
	} else if b.config.OSType == constants.Target_Windows {
		steps = []multistep.Step{
			NewStepCreateResourceGroup(azureClient, ui),
			NewStepValidateTemplate(azureClient, ui, KeyVault),
			NewStepDeployTemplate(azureClient, ui, KeyVault),
			NewStepGetCertificate(azureClient, ui),
			NewStepSetCertificate(b.config, ui),
			NewStepValidateTemplate(azureClient, ui, Windows),
			NewStepDeployTemplate(azureClient, ui, Windows),
			NewStepGetIPAddress(azureClient, ui),
			&communicator.StepConnectWinRM{
				Config: &b.config.Comm,
				Host: func(stateBag multistep.StateBag) (string, error) {
					return stateBag.Get(constants.SSHHost).(string), nil
				},
				WinRMConfig: func(multistep.StateBag) (*communicator.WinRMConfig, error) {
					return &communicator.WinRMConfig{
						Username: b.config.UserName,
						Password: b.config.tmpAdminPassword,
					}, nil
				},
			},
			&common.StepProvision{},
			NewStepGetOSDisk(azureClient, ui),
			NewStepPowerOffCompute(azureClient, ui),
			NewStepDeleteResourceGroup(azureClient, ui),
			NewStepDeleteOSDisk(azureClient, ui),
		}
	} else {
		return nil, fmt.Errorf("Builder does not support the os_type '%s'", b.config.OSType)
	}

	if b.config.PackerDebug {
		ui.Message(fmt.Sprintf("temp admin user: '%s'", b.config.UserName))
		ui.Message(fmt.Sprintf("temp admin password: '%s'", b.config.Password))
	}

	b.runner = b.createRunner(&steps, ui)
	b.runner.Run(b.stateBag)

	// Report any errors.
	if rawErr, ok := b.stateBag.GetOk(constants.Error); ok {
		return nil, rawErr.(error)
	}

	// If we were interrupted or cancelled, then just exit.
	if _, ok := b.stateBag.GetOk(multistep.StateCancelled); ok {
		return nil, errors.New("Build was cancelled.")
	}

	if _, ok := b.stateBag.GetOk(multistep.StateHalted); ok {
		return nil, errors.New("Build was halted.")
	}

	return &artifact{}, nil
}

func (b *Builder) Cancel() {
	if b.runner != nil {
		log.Println("Cancelling the step runner...")
		b.runner.Cancel()
	}
}

func (b *Builder) createRunner(steps *[]multistep.Step, ui packer.Ui) multistep.Runner {
	if b.config.PackerDebug {
		return &multistep.DebugRunner{
			Steps:   *steps,
			PauseFn: common.MultistepDebugFn(ui),
		}
	}

	return &multistep.BasicRunner{
		Steps: *steps,
	}
}

func (b *Builder) configureStateBag(stateBag multistep.StateBag) error {
	stateBag.Put(constants.AuthorizedKey, b.config.sshAuthorizedKey)
	stateBag.Put(constants.PrivateKey, b.config.sshPrivateKey)

	stateBag.Put(constants.ArmComputeName, b.config.tmpComputeName)
	stateBag.Put(constants.ArmDeploymentName, b.config.tmpDeploymentName)
	stateBag.Put(constants.ArmLocation, b.config.Location)
	stateBag.Put(constants.ArmResourceGroupName, b.config.tmpResourceGroupName)
	stateBag.Put(constants.ArmKeyVaultName, b.config.tmpKeyVaultName)
	stateBag.Put(constants.ArmTemplateParameters, b.config.toTemplateParameters())
	stateBag.Put(constants.ArmVirtualMachineCaptureParameters, b.config.toVirtualMachineCaptureParameters())

	stateBag.Put(constants.ArmPublicIPAddressName, DefaultPublicIPAddressName)

	return nil
}

func (b *Builder) getServicePrincipalTokens(say func(string)) (*azure.ServicePrincipalToken, *azure.ServicePrincipalToken, error) {
	var servicePrincipalToken *azure.ServicePrincipalToken
	var servicePrincipalTokenVault *azure.ServicePrincipalToken

	var err error

	if b.config.useDeviceLogin {
		servicePrincipalToken, err = packerAzureCommon.Authenticate(azure.PublicCloud, b.config.SubscriptionID, say)
		if err != nil {
			return nil, nil, err
		}
	} else {
		auth := NewAuthenticate(azure.PublicCloud, b.config.ClientID, b.config.ClientSecret, b.config.TenantID)

		servicePrincipalToken, err = auth.getServicePrincipalToken()
		if err != nil {
			return nil, nil, err
		}

		servicePrincipalTokenVault, err = auth.getServicePrincipalTokenWithResource(packerAzureCommon.AzureVaultScope)
		if err != nil {
			return nil, nil, err
		}
	}

	return servicePrincipalToken, servicePrincipalTokenVault, nil
}
