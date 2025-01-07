runner="./build/runner"
logsFolder="batchLogs"
networkTemplate="networks/template.toml"
replaceString="%VAR%"

# Make output folder if it doesn't exist
mkdir -p $logsFolder

echo Batch test;
echo values: "$@"
echo -----------------------------------
# Go through each value
for var in "$@"; do
	# Clear previous logs
	rm -rf networks/logs

	# Replace the marker with the value and mark the changed line
	sed -i -e "s/%VAR%/$var #%VAR%/" $networkTemplate

	# Run tests and generate graphs
	echo Running experiment with var=$var...
	echo -----------------------------------
	$runner -f $networkTemplate
	echo Generating graphs...
	echo -----------------------------------
	$runner -f $networkTemplate plot

	# Clear previous logs and move logs and graphs to experiment folder
	echo Moving results...
	echo -----------------------------------
	mkdir -p "$logsFolder/$var/"
	mv networks/logs "$logsFolder/$var/"
	mv *.png "$logsFolder/$var/"

	# Change var back to marker
	echo Cleanup...
	echo -----------------------------------
	sed -i -e "s/$var #%VAR%/%VAR%/" $networkTemplate
done

echo Experiment complete.
echo Values: "$@"
echo -----------------------------------
