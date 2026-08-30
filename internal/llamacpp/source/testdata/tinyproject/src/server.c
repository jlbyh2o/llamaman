/* A stand-in for llama-server that answers the three probes the `verify` phase
 * runs: D18's --version, D19's --list-devices, and the --help capture that
 * becomes help_flags_json and supports_fit. */
#include <stdio.h>
#include <string.h>

int tiny_build_number(void);

int main(int argc, char **argv) {
	const char *cmd = argc > 1 ? argv[1] : "";

	if (strcmp(cmd, "--version") == 0) {
		printf("version: %d (tiny)\n", tiny_build_number());
		printf("built with cc for x86_64-pc-linux-gnu\n");
		return 0;
	}
	if (strcmp(cmd, "--list-devices") == 0) {
		printf("Available devices:\n");
		return 0;
	}
	if (strcmp(cmd, "--help") == 0) {
		printf("----- common params -----\n\n");
		printf("-h,    --help, --usage          print usage and exit\n");
		printf("-c,    --ctx-size N             size of the prompt context\n");
		printf("       --fit                    let llama.cpp choose the offload\n");
		return 0;
	}
	return 0;
}
