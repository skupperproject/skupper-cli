# skupper-cli

Command line tool for setting up and managing skupper installations

Uses (and expects) current context from existing kube config. Will
then create, delet or update a skupper installation.

## Usage

### skupper init

Creates a skupper installation. Options are:

--hub   indicates that this installation can be configured to allow
        other skupper installations to connect to it

--name  name of the installation

### skupper delete

Deletes a skupper installation.

### skupper secret

Generates a kubernetes secret (as yaml) which can be used in another
skupper installation to connect to this one. Options are:

--subject  the CN to write into the certificate

--file     the path to the file in which to write the yaml

### skupper connect

Connects this skupper installation to the installation by which the
specified secret was generated. Options are:

--name    the name for the connection

--secret  the path to the file in which the secret is held

## Example

In one kubernetes context do:

```
skupper init --hub --name site1
skupper secret --file /path/to/mysecret.yaml --subject site2
```

In another context, e.g. another kubernetes cluster, do:

```
skupper init --name site2
skupper connect --secret /path/to/mysecret.yaml --name site1
```





