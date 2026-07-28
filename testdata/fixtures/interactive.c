#include <stdio.h>
#include <string.h>

int main(void)
{
	char line[128];
	int n = 0;

	/* No trailing newline: proves the pty keeps libc line-buffered. */
	printf("name? ");
	fflush(stdout);
	if (!fgets(line, sizeof line, stdin))
		return 1;
	line[strcspn(line, "\n")] = '\0';
	printf("hello %s\n", line);

	printf("count? ");
	fflush(stdout);
	if (scanf("%d", &n) != 1)
		return 1;
	printf("count=%d\n", n);
	return 0;
}
