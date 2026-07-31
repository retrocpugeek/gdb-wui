/* Built without -g, deliberately not stripped, so its globals survive in the
 * ELF symbol table with an address but no type. That combination is what a
 * release firmware image looks like, and it is the case where gdb refuses to
 * evaluate a symbol — "'LogType' has unknown type; cast it to its declared
 * type" — while still knowing perfectly well where it lives. */
#include <stdio.h>

int LogType = 7;
char LogBuffer[64] = "ready";
static int LogCount;

int LogWrite(const char *msg)
{
	LogCount++;
	snprintf(LogBuffer, sizeof LogBuffer, "%d:%s", LogType, msg);
	return LogCount;
}

int main(void)
{
	LogWrite("start");
	printf("%s\n", LogBuffer);
	return 0;
}
