*** Settings ***
Documentation  Test 5-1 - Distributed Switch
Resource  ../../resources/Util.robot
Suite Teardown  Run Keyword And Ignore Error  Nimbus Cleanup

*** Test Cases ***
Test
    Log To Console  \nStarting test...
    ${esx1}  ${esx1-ip}=  Deploy Nimbus ESXi Server  %{NIMBUS_USER}  %{NIMBUS_PASSWORD}
    Set Suite Variable  ${ESX1}  ${esx1}
    ${esx2}  ${esx2-ip}=  Deploy Nimbus ESXi Server  %{NIMBUS_USER}  %{NIMBUS_PASSWORD}
    Set Suite Variable  ${ESX2}  ${esx2}
    ${esx3}  ${esx3-ip}=  Deploy Nimbus ESXi Server  %{NIMBUS_USER}  %{NIMBUS_PASSWORD}
    Set Suite Variable  ${ESX3}  ${esx3}

    ${vc}  ${vc-ip}=  Deploy Nimbus vCenter Server  %{NIMBUS_USER}  %{NIMBUS_PASSWORD}
    Set Suite Variable  ${VC}  ${vc}

    Log To Console  Create a datacenter on the VC
    ${out}=  Run  govc datacenter.create ha-datacenter
    Should Be Empty  ${out}

    Log To Console  Add ESX host to the VC
    ${out}=  Run  govc host.add -hostname=${esx1-ip} -username=root -dc=ha-datacenter -password=e2eFunctionalTest -noverify=true
    Should Contain  ${out}  OK
    ${out}=  Run  govc host.add -hostname=${esx2-ip} -username=root -dc=ha-datacenter -password=e2eFunctionalTest -noverify=true
    Should Contain  ${out}  OK
    ${out}=  Run  govc host.add -hostname=${esx3-ip} -username=root -dc=ha-datacenter -password=e2eFunctionalTest -noverify=true
    Should Contain  ${out}  OK

    Create A Distributed Switch  ha-datacenter

    Create Three Distributed Port Groups  ha-datacenter

    Add Host To Distributed Switch  ${esx1-ip}
    Add Host To Distributed Switch  ${esx2-ip}
    Add Host To Distributed Switch  ${esx3-ip}

    Log To Console  Deploy VIC to the VC cluster
    Set Environment Variable  TEST_URL_ARRAY  ${vc-ip}
    Set Environment Variable  TEST_USERNAME  Administrator@vsphere.local
    Set Environment Variable  TEST_PASSWORD  Admin\!23
    Set Environment Variable  BRIDGE_NETWORK  bridge
    Set Environment Variable  EXTERNAL_NETWORK  vm-network
    Set Environment Variable  TEST_RESOURCE  /ha-datacenter/host/${esx1-ip}/Resources
    Set Environment Variable  TEST_TIMEOUT  30m

    Install VIC Appliance To Test Server

    Run Regression Tests
