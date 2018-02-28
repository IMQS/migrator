# Database migrator

This program consumes a directory containing one or more files with an extension of `.sql`.
The lexical order of the files determines the order of the migrations. Once a migration
runs successfully, it's name is stored inside a table called `schema_migrations`.

If the database does not exist, then the migration will create the database.


