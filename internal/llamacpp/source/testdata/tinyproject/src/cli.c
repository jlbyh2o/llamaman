/* A stand-in for llama-cli: present, executable, and linked against the shared
 * library, which is all section 6.5's `install` assertion asks of it. */
#include <stdio.h>

int tiny_build_number(void);

int main(void) {
	printf("version: %d (tiny)\n", tiny_build_number());
	return 0;
}
