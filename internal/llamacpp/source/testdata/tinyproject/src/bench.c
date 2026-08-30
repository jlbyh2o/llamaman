/* A stand-in for llama-bench. Like the real one it has its own argument parser
 * and exits NON-ZERO on a flag it does not know, which is exactly why section
 * 6.4 accepts it on its output rather than on its exit status. */
#include <stdio.h>

int tiny_build_number(void);

int main(void) {
	printf("usage: llama-bench [options] (build %d)\n", tiny_build_number());
	return 1;
}
