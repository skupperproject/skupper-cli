# skupper-cli

Command line tool for setting up and managing skupper installations

## Usage

See `skupper help` or `skupper <command> --help` for details.

## Example

In one kubernetes context do:

```
skupper init
skupper connection-token /path/to/mysecret.yaml
```

In another context, e.g. another kubernetes cluster, do:

```
skupper init
skupper connect --secret /path/to/mysecret.yaml
```




