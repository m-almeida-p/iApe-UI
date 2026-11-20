#!/bin/sh
# A stand-in for yape-token, for the scripted BFF (cmd/test-bff): it prints a token that the scripted BFF accepts. Use it as
# YAPE_TOKEN_BIN when the Web UI talks to the scripted BFF and no Yape account is at hand. It prints no real secret.
echo test-token
